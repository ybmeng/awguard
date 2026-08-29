// Models.swift — Swift mirror of the spec in go/botnet/schema.go.
// The Go structs are the single spec; these types keep the same shape and JSON
// keys so a future switch from local persistence to the Go service API is a
// transport change, not a model change.

import Foundation

struct Bot: Identifiable, Codable, Hashable {
    var id: String            // "bot_" + ULID
    var displayName: String
    var createdAt: Date
    var systemPrompt: String
    var model: String         // universal ModelID, e.g. "openrouter/deepseek/deepseek-v4"
}

// Chat history is referenced by bot, never embedded in it (schema.go DECISION):
// messages persist per-bot, keyed by botId.
struct Message: Identifiable, Codable, Hashable {
    var id: String            // "msg_" + ULID → time-sortable
    var botId: String
    var role: Role
    var content: String
    var sentAt: Date

    enum Role: String, Codable {
        case user, bot, system
    }
}

// Mirror of go/lib/modelSelector: universal ID = "openrouter/<slug>", where the
// slug after the provider prefix is what the OpenRouter API takes as `model`.
struct ModelOption: Identifiable, Hashable {
    var name: String
    var id: String

    var openRouterSlug: String {
        id.hasPrefix("openrouter/") ? String(id.dropFirst("openrouter/".count)) : id
    }

    static let roster: [ModelOption] = [
        ModelOption(name: "DeepSeek V4", id: "openrouter/deepseek/deepseek-v4"),
        ModelOption(name: "GLM 5.3 Flash", id: "openrouter/z-ai/glm-5.3-flash"),
    ]
}

enum ULID {
    private static let crockford = Array("0123456789ABCDEFGHJKMNPQRSTVWXYZ")

    /// Prefixed, lexicographically sortable id matching go/botnet/id.go:
    /// 48-bit millisecond timestamp + 80 random bits, Crockford base32.
    static func new(_ prefix: String) -> String {
        var bytes = [UInt8](repeating: 0, count: 16)
        let ms = UInt64(Date().timeIntervalSince1970 * 1000)
        for i in 0..<6 { bytes[i] = UInt8(truncatingIfNeeded: ms >> (40 - 8 * UInt64(i))) }
        for i in 6..<16 { bytes[i] = UInt8.random(in: .min ... .max) }

        var value: UInt32 = 0
        var bits = 0
        var out = ""
        out.reserveCapacity(26)
        for b in bytes {
            value = (value << 8) | UInt32(b)
            bits += 8
            while bits >= 5 {
                bits -= 5
                out.append(crockford[Int((value >> UInt32(bits)) & 31)])
            }
        }
        // 128 bits = 25 chars * 5 + 3 leftover bits, pad like the canonical ULID
        // encoding by flushing the remainder.
        if bits > 0 {
            out.append(crockford[Int((value << UInt32(5 - bits)) & 31)])
        }
        return prefix + out
    }
}
