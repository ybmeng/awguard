// Models.swift — Swift mirror of go/botnet/schema.go, decoded from the server's
// JSON. The app holds no state of its own; these are just the shapes botnetd
// returns.

import Foundation

struct Bot: Identifiable, Codable, Hashable {
    var id: String            // "bot_" + ULID (assigned by the server)
    var displayName: String
    var createdAt: Date
    var systemPrompt: String
    var model: String         // universal ModelID, e.g. "openrouter/deepseek/deepseek-v4"
}

struct Message: Identifiable, Codable, Hashable {
    var id: String            // "msg_" + ULID
    var botId: String
    var role: Role
    var content: String
    var sentAt: Date

    enum Role: String, Codable {
        case user, bot, system
    }
}

// Decoded from GET /v1/models ({name, id}); mirrors go/lib/modelSelector.
struct ModelOption: Identifiable, Codable, Hashable {
    var name: String
    var id: String

    static let roster: [ModelOption] = [
        ModelOption(name: "DeepSeek V4", id: "openrouter/deepseek/deepseek-v4"),
        ModelOption(name: "GLM 5.3 Flash", id: "openrouter/z-ai/glm-5.3-flash"),
    ]
}
