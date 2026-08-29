// Models.swift — Swift mirror of go/botnet/schema.go, decoded from the server's
// JSON. The app holds no state of its own; these are just the shapes botnetd
// returns.
//
// Fields the server only started sending recently are optional here, so one
// build of the app works against both an old and a new botnetd. Treat a nil as
// "this server doesn't know about it yet", never as a real value.

import Foundation

// Go marshals a zero time.Time as "0001-01-01T00:00:00Z" rather than null, so an
// absent timestamp arrives as a year-1 date instead of nil. Everything that
// reads a server timestamp goes through this.
extension Date {
    private static let goZeroCutoff = Date(timeIntervalSince1970: 0)
    var isServerZero: Bool { self < Self.goZeroCutoff }
    var nilIfServerZero: Date? { isServerZero ? nil : self }
}

struct Bot: Identifiable, Codable, Hashable {
    var id: String            // "bot_" + ULID (assigned by the server)
    var displayName: String
    var createdAt: Date
    var systemPrompt: String
    var model: String         // universal ModelID, e.g. "openrouter/deepseek/deepseek-v4"

    // Sidebar metadata, denormalized by the server so the list draws without
    // fetching every conversation.
    var lastMessageAt: Date?
    var lastMessageText: String?
    var readAt: Date?

    // False when the bot's model has left the roster: it still lists, but every
    // send will fail until the model is changed.
    var modelValid: Bool?

    // The bot's editable memory. Nil on a server that predates it; "" is a real
    // value meaning cleared, so the UI treats both as empty.
    var memory: String?

    /// Nil until the bot has any message, on both old and new servers.
    var lastActivity: Date? { lastMessageAt?.nilIfServerZero }

    var hasUnread: Bool {
        guard let lastActivity else { return false }
        guard let seen = readAt?.nilIfServerZero else { return true }
        return lastActivity > seen
    }

    /// Nil on a server that doesn't report validity, which is not the same as
    /// known-bad, so callers must not warn on it.
    var isModelBroken: Bool { modelValid == false }
}

struct Message: Identifiable, Codable, Hashable {
    var id: String            // "msg_" + ULID
    var botId: String
    var segmentId: String?
    var role: Role
    var content: String
    var sentAt: Date
    var status: Status?
    var error: String?

    enum Role: String, Codable {
        case user, bot, system
    }

    // A user turn whose model call failed is stranded: it is persisted with no
    // reply, and only the status tells it apart from one still in flight.
    enum Status: String, Codable {
        case sent, awaiting, failed
    }

    var didFail: Bool { status == .failed }

    /// A client-side stand-in for a turn the server has not acknowledged yet.
    /// The id is deliberately not a "msg_" id: nothing may look this up on the
    /// server, and it is replaced by the real message as soon as one arrives.
    static func placeholder(content: String, botID: String) -> Message {
        Message(
            id: "pending_\(UUID().uuidString)",
            botId: botID,
            segmentId: nil,
            role: .user,
            content: content,
            sentAt: Date(),
            status: .awaiting,
            error: nil
        )
    }

    var isPlaceholder: Bool { id.hasPrefix("pending_") }
}

// One link in a bot's conversation chain. Exactly one segment is open (nil
// sealedAt) and new messages append to it; compacting seals it with a
// cumulative summary and opens a fresh one.
struct Segment: Identifiable, Codable, Hashable {
    var id: String            // "seg_" + ULID
    var botId: String
    var index: Int
    var openedAt: Date
    var sealedAt: Date?
    var summary: String?

    var sealed: Date? { sealedAt?.nilIfServerZero }
    var isOpen: Bool { sealed == nil }
}

// Decoded from GET /v1/tools: the exact tool definitions the server sends the
// model with every turn, in OpenAI function-calling shape
// ({type, function: {name, description, parameters}}). Shown so the user can
// read precisely what the model is told; nothing here is paraphrased.
struct ToolDefinition: Identifiable, Decodable, Hashable {
    var name: String
    /// Multiline prose, kept verbatim — line breaks and all.
    var description: String
    /// The parameters JSON Schema, re-indented for display. Decoded opaquely
    /// (the schema's shape will evolve), never into typed fields.
    var parametersJSON: String

    var id: String { name }

    private enum CodingKeys: String, CodingKey { case function }
    private enum FunctionKeys: String, CodingKey { case name, description, parameters }

    init(from decoder: Decoder) throws {
        let outer = try decoder.container(keyedBy: CodingKeys.self)
        let fn = try outer.nestedContainer(keyedBy: FunctionKeys.self, forKey: .function)
        name = try fn.decode(String.self, forKey: .name)
        description = try fn.decode(String.self, forKey: .description)
        parametersJSON = try fn.decode(JSONValue.self, forKey: .parameters).prettyPrinted
    }
}

/// Arbitrary JSON decoded without a schema, for server fields whose shape is
/// deliberately not part of the app's contract. Display-only: it re-serializes
/// to indented text rather than offering typed access.
enum JSONValue: Decodable, Hashable {
    case object([String: JSONValue])
    case array([JSONValue])
    case string(String)
    case number(Double)
    case bool(Bool)
    case null

    // Bool must be probed before Double: JSON true/false may otherwise bridge
    // through NSNumber and decode as 1/0.
    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null }
        else if let b = try? c.decode(Bool.self) { self = .bool(b) }
        else if let n = try? c.decode(Double.self) { self = .number(n) }
        else if let s = try? c.decode(String.self) { self = .string(s) }
        else if let a = try? c.decode([JSONValue].self) { self = .array(a) }
        else if let o = try? c.decode([String: JSONValue].self) { self = .object(o) }
        else {
            throw DecodingError.dataCorruptedError(
                in: c, debugDescription: "unrecognized JSON value")
        }
    }

    private var foundation: Any {
        switch self {
        case .object(let o): return o.mapValues(\.foundation)
        case .array(let a): return a.map(\.foundation)
        case .string(let s): return s
        // Integral doubles go back out as integers so `"maxLength": 3` doesn't
        // display as 3.0 after the round trip.
        case .number(let n): return n == n.rounded() && abs(n) < 1e15 ? Int64(n) as Any : n
        case .bool(let b): return b
        case .null: return NSNull()
        }
    }

    var prettyPrinted: String {
        let obj = foundation
        guard JSONSerialization.isValidJSONObject(obj),
              let data = try? JSONSerialization.data(
                withJSONObject: obj, options: [.prettyPrinted, .sortedKeys]),
              let text = String(data: data, encoding: .utf8)
        else { return String(describing: obj) }
        return text
    }
}

// Decoded from GET /v1/models ({name, id}); mirrors go/lib/modelSelector.
struct ModelOption: Identifiable, Codable, Hashable {
    var name: String
    var id: String

    static let roster: [ModelOption] = [
        ModelOption(name: "DeepSeek V4 Flash", id: "openrouter/deepseek/deepseek-v4-flash-0731"),
        ModelOption(name: "GLM 5.3 Flash", id: "openrouter/z-ai/glm-5.3-flash"),
    ]
}
