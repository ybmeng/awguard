// Store.swift — thin client over the Go botnet server. Holds NO durable state:
// every read and every mutation is an HTTP call to botnetd, which owns the
// bots, the messages, and the OpenRouter calls. This object only caches the
// server's responses for the views to render.

import Foundation

@MainActor
final class AppStore: ObservableObject {
    @Published private(set) var bots: [Bot] = []
    @Published private(set) var conversations: [String: [Message]] = [:] // botId → messages
    @Published private(set) var segments: [String: [Segment]] = [:]      // botId → chain
    @Published private(set) var models: [ModelOption] = ModelOption.roster
    @Published var pendingBotIDs: Set<String> = []
    @Published var compactingBotIDs: Set<String> = []
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
            return
        }
        await prefetchConversations()
    }

    // A server that denormalizes the preview onto the bot has already told us
    // everything the sidebar needs. Only fall back to fetching every
    // conversation for one that hasn't, which costs a request per bot.
    private func prefetchConversations() async {
        let stale = bots.filter { $0.lastMessageText == nil && conversations[$0.id] == nil }
        guard !stale.isEmpty else { return }
        let api = self.api
        let ids = stale.map(\.id)
        let loaded = await withTaskGroup(of: (String, [Message]?).self) { group in
            for id in ids {
                group.addTask { (id, try? await api.messages(id)) }
            }
            var out: [String: [Message]] = [:]
            for await (id, messages) in group {
                if let messages { out[id] = messages }
            }
            return out
        }
        for (id, messages) in loaded { conversations[id] = messages }
    }

    /// The sidebar's one-line preview, preferring the server's denormalized copy
    /// and falling back to a loaded conversation.
    func preview(for bot: Bot) -> String? {
        if let text = bot.lastMessageText, !text.isEmpty { return text }
        return conversations[bot.id]?.last?.content
    }

    func lastActivity(for bot: Bot) -> Date? {
        bot.lastActivity ?? conversations[bot.id]?.last?.sentAt
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

    func lastMessage(for botID: String) -> Message? { conversations[botID]?.last }

    func segmentChain(for botID: String) -> [Segment] { segments[botID] ?? [] }

    // Segments, compaction, read state and bot edits all landed after the first
    // release of the server. On a botnetd that predates them the route is a 404,
    // which means "this server can't do that" and must stay silent rather than
    // raising an error the user can do nothing about.
    func loadSegments(_ botID: String) async {
        do {
            segments[botID] = try await api.segments(botID)
        } catch {
            guard !APIClient.isUnimplemented(error) else { return }
            lastError = error.localizedDescription
        }
    }

    func compact(_ bot: Bot) async {
        guard !compactingBotIDs.contains(bot.id) else { return }
        compactingBotIDs.insert(bot.id)
        defer { compactingBotIDs.remove(bot.id) }
        do {
            segments[bot.id] = try await api.compact(bot.id)
            conversations[bot.id] = try await api.messages(bot.id)
        } catch {
            lastError = APIClient.isUnimplemented(error)
                ? "This botnetd is too old to compact — restart it from the current build."
                : error.localizedDescription
        }
    }

    func markRead(_ bot: Bot) async {
        guard bot.hasUnread else { return }
        do {
            let updated = try await api.markRead(bot.id)
            if let i = bots.firstIndex(where: { $0.id == updated.id }) { bots[i] = updated }
        } catch {
            guard !APIClient.isUnimplemented(error) else { return }
            lastError = error.localizedDescription
        }
    }

    func updateBot(_ bot: Bot, fields: [String: String]) async {
        do {
            _ = try await api.patchBot(bot.id, fields: fields)
            bots = try await api.listBots()
        } catch {
            lastError = APIClient.isUnimplemented(error)
                ? "This botnetd is too old to edit a bot — restart it from the current build."
                : error.localizedDescription
        }
    }

    func hasServerKey() async throws -> Bool { try await api.hasKey() }

    func setServerKey(_ key: String) async {
        do { try await api.setKey(key) } catch { lastError = error.localizedDescription }
    }

    // The chat lifecycle lives server-side; the client just posts and renders
    // whatever conversation comes back. A failed send is not an exception here:
    // the server persists the user's turn either way and hands back the
    // transcript with it, so the stranded message renders in place and the
    // failure shows on the turn rather than in a modal.
    func send(_ text: String, to bot: Bot) async {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }

        // Draw the user's turn before the request goes out. The send blocks for
        // as long as the model takes — measured at 8s — and until it returns the
        // message exists nowhere the transcript can render, so the UI looked
        // like it had swallowed the text. This placeholder is replaced by the
        // server's own copy, id and all, the moment anything comes back.
        let placeholder = Message.placeholder(content: trimmed, botID: bot.id)
        conversations[bot.id, default: []].append(placeholder)

        pendingBotIDs.insert(bot.id)
        defer { pendingBotIDs.remove(bot.id) }
        do {
            let persisted = try await api.send(trimmed, to: bot.id)
            replace(placeholder.id, with: persisted, in: bot.id)
            await awaitReply(to: persisted, on: bot.id)
        } catch {
            lastError = error.localizedDescription
            // Replace wholesale rather than dropping the placeholder, so the
            // transcript matches the server even if it did persist the turn.
            conversations[bot.id] = try? await api.messages(bot.id)
        }
    }

    func retry(_ messageID: String, on botID: String) async {
        pendingBotIDs.insert(botID)
        defer { pendingBotIDs.remove(botID) }
        do {
            let reopened = try await api.retry(messageID, on: botID)
            replace(messageID, with: reopened, in: botID)
            await awaitReply(to: reopened, on: botID)
        } catch {
            lastError = error.localizedDescription
            conversations[botID] = try? await api.messages(botID)
        }
    }

    /// Polls until the turn settles. The send returns in milliseconds now, so
    /// this is where the model's actual latency is spent; the turn stays
    /// `awaiting` in the transcript throughout, which is what the user sees.
    ///
    /// Giving up on the deadline deliberately leaves the message awaiting rather
    /// than faking a failure: the server owns that state, and its startup sweep
    /// or an explicit retry is what settles it.
    private func awaitReply(to turn: Message, on botID: String) async {
        let deadline = Date().addingTimeInterval(Self.replyTimeout)
        while Date() < deadline {
            try? await Task.sleep(nanoseconds: 600_000_000)
            guard let settled = try? await api.message(turn.id) else { continue }
            guard settled.status != .awaiting else { continue }

            replace(turn.id, with: settled, in: botID)
            if let reply = try? await api.messages(botID, after: turn.id) {
                appendMissing(reply, in: botID)
            }
            await refreshBotList()
            return
        }
    }

    private static let replyTimeout: TimeInterval = 180

    private func replace(_ messageID: String, with message: Message, in botID: String) {
        guard var thread = conversations[botID] else { return }
        if let i = thread.firstIndex(where: { $0.id == messageID }) {
            thread[i] = message
        } else {
            thread.append(message)
        }
        conversations[botID] = thread
    }

    private func appendMissing(_ incoming: [Message], in botID: String) {
        var thread = conversations[botID] ?? []
        let known = Set(thread.map(\.id))
        thread.append(contentsOf: incoming.filter { !known.contains($0.id) })
        conversations[botID] = thread
    }

    // The sidebar's preview and ordering are server-derived, so they go stale on
    // every send until the bot list is re-read.
    private func refreshBotList() async {
        bots = (try? await api.listBots()) ?? bots
    }
}
