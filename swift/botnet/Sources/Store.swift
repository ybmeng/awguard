// Store.swift — thin client over the Go botnet server. Holds NO durable state:
// every read and every mutation is an HTTP call to botnetd, which owns the
// bots, the messages, and the OpenRouter calls. This object only caches the
// server's responses for the views to render.

import Foundation

@MainActor
final class AppStore: ObservableObject {
    @Published private(set) var bots: [Bot] = []
    @Published private(set) var conversations: [String: [Message]] = [:] // botId → messages
    @Published private(set) var models: [ModelOption] = ModelOption.roster
    @Published var pendingBotIDs: Set<String> = []
    @Published var lastError: String?
    @Published var serverReachable = true

    private let api = APIClient()

    func refresh() async {
        do {
            bots = try await api.listBots()
            models = try await api.listModels()
            serverReachable = true
        } catch {
            serverReachable = false
            lastError = "Can't reach botnetd — is the server running? (\(error.localizedDescription))"
        }
    }

    func createBot(displayName: String, systemPrompt: String, model: String) async {
        do {
            _ = try await api.createBot(displayName: displayName, systemPrompt: systemPrompt, model: model)
            bots = try await api.listBots()
        } catch {
            lastError = error.localizedDescription
        }
    }

    func deleteBot(_ bot: Bot) async {
        do {
            try await api.deleteBot(bot.id)
            conversations[bot.id] = nil
            bots = try await api.listBots()
        } catch {
            lastError = error.localizedDescription
        }
    }

    func loadConversation(_ botID: String) async {
        do {
            conversations[botID] = try await api.messages(botID)
        } catch {
            lastError = error.localizedDescription
        }
    }

    func messages(for botID: String) -> [Message] { conversations[botID] ?? [] }

    func hasServerKey() async throws -> Bool { try await api.hasKey() }

    func setServerKey(_ key: String) async {
        do { try await api.setKey(key) } catch { lastError = error.localizedDescription }
    }

    // The chat lifecycle lives server-side; the client just posts and renders
    // whatever conversation comes back.
    func send(_ text: String, to bot: Bot) async {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        pendingBotIDs.insert(bot.id)
        defer { pendingBotIDs.remove(bot.id) }
        do {
            conversations[bot.id] = try await api.send(trimmed, to: bot.id)
        } catch {
            lastError = error.localizedDescription
            // reload so the user message the server did persist still shows
            conversations[bot.id] = try? await api.messages(bot.id)
        }
    }
}
