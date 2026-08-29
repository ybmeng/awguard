// Store.swift — local persistence + the chat lifecycle.
//
// Layout mirrors the schema.go decisions: bots are small metadata in one file,
// chat history is referenced per bot in its own file (the message is the
// storage unit):
//   Documents/botnet/bots.json
//   Documents/botnet/messages-<botID>.json
//
// CHAT LIFECYCLE (the declared contract, no compaction):
//   1. user sends text → append Message(role: user) → persist
//   2. call OpenRouter with systemPrompt + the FULL history (every user/bot
//      message, in order; no truncation, no summarization)
//   3. reply → append Message(role: bot) → persist
//   4. on failure → nothing is appended for the model; the user message stays,
//      the error surfaces in the UI, and retrying just sends again
// Every message is on disk before and after the network call, so a crash
// mid-request loses nothing that was said.

import Foundation

@MainActor
final class AppStore: ObservableObject {
    @Published private(set) var bots: [Bot] = []
    @Published private(set) var conversations: [String: [Message]] = [:] // botId → messages
    @Published var pendingBotIDs: Set<String> = []                       // awaiting a model reply
    @Published var lastError: String?

    private let dir: URL
    private let encoder: JSONEncoder = {
        let e = JSONEncoder()
        e.dateEncodingStrategy = .iso8601
        e.outputFormatting = [.prettyPrinted, .sortedKeys]
        return e
    }()
    private let decoder: JSONDecoder = {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .iso8601
        return d
    }()

    init() {
        dir = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("botnet", isDirectory: true)
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        load()
    }

    // MARK: bots

    func createBot(displayName: String, systemPrompt: String, model: String) {
        let bot = Bot(
            id: ULID.new("bot_"),
            displayName: displayName,
            createdAt: Date(),
            systemPrompt: systemPrompt,
            model: model
        )
        bots.append(bot)
        saveBots()
    }

    func deleteBot(_ bot: Bot) {
        bots.removeAll { $0.id == bot.id }
        conversations[bot.id] = nil
        try? FileManager.default.removeItem(at: messagesURL(bot.id))
        saveBots()
    }

    func messages(for botID: String) -> [Message] {
        conversations[botID] ?? []
    }

    // MARK: chat lifecycle

    func send(_ text: String, to bot: Bot) async {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }

        // 1. append + persist the user message before anything touches the network
        append(Message(id: ULID.new("msg_"), botId: bot.id, role: .user, content: trimmed, sentAt: Date()))

        // 2. full history to the model, no compaction
        pendingBotIDs.insert(bot.id)
        defer { pendingBotIDs.remove(bot.id) }
        do {
            let client = OpenRouterClient(apiKey: Keychain.apiKey)
            let reply = try await client.complete(bot: bot, history: messages(for: bot.id))
            // 3. append + persist the reply
            append(Message(id: ULID.new("msg_"), botId: bot.id, role: .bot, content: reply, sentAt: Date()))
        } catch {
            // 4. surface the failure; the user message already persisted
            lastError = error.localizedDescription
        }
    }

    private func append(_ msg: Message) {
        conversations[msg.botId, default: []].append(msg)
        saveMessages(botID: msg.botId)
    }

    // MARK: persistence

    private func botsURL() -> URL { dir.appendingPathComponent("bots.json") }
    private func messagesURL(_ botID: String) -> URL {
        dir.appendingPathComponent("messages-\(botID).json")
    }

    private func load() {
        if let data = try? Data(contentsOf: botsURL()),
           let loaded = try? decoder.decode([Bot].self, from: data) {
            bots = loaded
        }
        for bot in bots {
            if let data = try? Data(contentsOf: messagesURL(bot.id)),
               let msgs = try? decoder.decode([Message].self, from: data) {
                conversations[bot.id] = msgs
            }
        }
    }

    private func saveBots() {
        if let data = try? encoder.encode(bots) {
            try? data.write(to: botsURL(), options: .atomic)
        }
    }

    private func saveMessages(botID: String) {
        if let data = try? encoder.encode(conversations[botID] ?? []) {
            try? data.write(to: messagesURL(botID), options: .atomic)
        }
    }
}
