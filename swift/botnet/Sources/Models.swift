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

    // The web sources a bot reply drew on, in the order OpenRouter returned
    // them. Present only on bot replies that used web search, and omitted from
    // the JSON otherwise (the common turn), so it decodes as nil on both an old
    // server and an ordinary reply — never a real value, per the header rule.
    var citations: [Citation]?

    // The tools this reply used, in the order the model called them — the audit
    // trail behind the answer. Present only on a bot reply that ran a tool, and
    // omitted from the JSON otherwise, so it decodes as nil on both an old
    // server and an ordinary reply. `citations` above stays the aggregate of
    // this turn's web_search results, so the Sources row is unchanged; this is
    // the additive, per-call surface.
    var toolCalls: [ToolCall]?

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
            error: nil,
            citations: nil,
            toolCalls: nil
        )
    }

    var isPlaceholder: Bool { id.hasPrefix("pending_") }
}

// One web source behind a bot reply — mirrors go/botnet/schema.go's Citation.
// The server always sends url and title (title falls back to the url host
// there); snippet and the index pair are omitted when unset. The indices point
// into the reply content for a later inline-superscript refinement the sources
// row doesn't use yet. title stays Optional here so a host fallback is always
// available even if a source ever arrives without one.
struct Citation: Codable, Hashable {
    var url: String
    var title: String?
    var snippet: String?
    var startIndex: Int?
    var endIndex: Int?

    /// What the sources row labels the link: the title, else the bare host, else
    /// the raw url — so a link is never blank.
    var displayTitle: String {
        if let title, !title.isEmpty { return title }
        return host ?? url
    }

    /// Bare host for the trailing label and the title fallback, minus a leading
    /// "www." so "www.example.com" reads as "example.com".
    var host: String? {
        guard let h = URLComponents(string: url)?.host else { return nil }
        return h.hasPrefix("www.") ? String(h.dropFirst(4)) : h
    }

    var link: URL? { URL(string: url) }
}

// One recorded tool invocation behind a bot reply — mirrors go/botnet/schema.go's
// ToolCall, the shared audit shape. `name` is the tool ("web_search", "memory"),
// `arguments` the raw JSON the model sent, `result` the string fed back to it
// (truncated server-side for a large search dump). `backend`, `results`, and
// `requestId` ride only on a web_search call, so all are Optional; `at` is
// display-optional too, so a fixture that omits it still decodes. `requestId` is
// the provider's request/response id (empty for memory and providers exposing
// none) — surfaced in the expanded row for debugging. The transcript's tool-call
// list decodes these; nothing here is a real value on an old server (the key is
// absent there), per the Models header rule.
struct ToolCall: Codable, Hashable {
    var name: String
    var arguments: String
    var result: String
    var backend: String?
    var results: [Citation]?
    var requestId: String?
    var at: Date?

    /// The web_search query, pulled from the raw arguments JSON. Nil when the
    /// call carried none or the args don't parse — the summary falls back then.
    var query: String? { argument("query") }

    /// The memory command ("read"/"replace"/"clear"), parsed the same way.
    var command: String? { argument("command") }

    /// The structured web sources this call returned; empty when it wasn't a
    /// search or found nothing. Feeds the row's expanded results, in order.
    var citations: [Citation] { results ?? [] }

    private func argument(_ key: String) -> String? {
        guard let data = arguments.data(using: .utf8),
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let value = obj[key] as? String, !value.isEmpty
        else { return nil }
        return value
    }
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

// One named calendar — mirrors go/botnet/schema.go's Calendar. Named
// EventCalendar because Foundation.Calendar is all over the date math in the
// calendar views, and shadowing it would force the namespace onto every
// `Calendar.current`. `color` is the server's enum string ("blue"…"teal");
// Palette.calendar(_:) maps it to a token and absorbs any value this build
// doesn't know. Calendars are last-write-wins like events.
struct EventCalendar: Identifiable, Codable, Hashable {
    var id: String            // "cal_" + ULID
    var name: String
    var color: String
    /// Whether events on this calendar may fire automations. Nil means an older
    /// botnetd, which cannot fire anything, so absent reads as not-executable.
    var executable: Bool?
    /// The bot id that created it, or `Event.userAuthor` for one made in the UI.
    var createdBy: String
    var createdAt: Date
    var updatedAt: Date

    var isExecutable: Bool { executable == true }

    /// The server's color enum, in the order it cycles them for an unnamed
    /// color. The recolor menus iterate this; Palette.calendar(_:) tolerates a
    /// value beyond it.
    static let colors = ["blue", "green", "orange", "purple", "red", "teal"]
}

// One calendar entry — mirrors go/botnet/schema.go's Event. The server sends
// location and notes with omitempty, so an unset one is absent from the JSON
// rather than "": both are Optional here and read as empty either way. Events
// are not If-Match versioned; calendar edits are last-write-wins like memory.
struct Event: Identifiable, Codable, Hashable {
    var id: String            // "evt_" + ULID, sortable
    var title: String
    var startsAt: Date
    var endsAt: Date
    var location: String?
    var notes: String?
    /// The calendar this event files under. Always present from a server with
    /// multiple calendars; nil means an older botnetd, never a real value.
    var calendarId: String?
    /// RFC 5545 recurrence rule (the server's supported subset). Omitted for a
    /// single event and by an older botnetd; startsAt/endsAt are the FIRST
    /// occurrence when set. The UI shows it read-only — bots and the
    /// registration tick author these, per the firing contract.
    var rrule: String?
    /// IANA timezone the rule expands in; present exactly when rrule is.
    var tz: String?
    /// The automation this event fires while an instance is active. Only legal
    /// on an executable calendar; omitted (nil) everywhere else.
    var automation: String?
    /// The bot id that created it, or `Event.userAuthor` for one made in the UI.
    var createdBy: String
    var createdAt: Date
    var updatedAt: Date

    var isRecurring: Bool { !(rrule ?? "").isEmpty }
    var firesAutomation: Bool { !(automation ?? "").isEmpty }

    /// The server's marker for an event the user made rather than a bot; it is
    /// not a bot id, so nothing may look it up in the bot list.
    static let userAuthor = "user"

    var isUserCreated: Bool { createdBy == Self.userAuthor }

    /// The day the event is filed under. Grouping keys off the start, so an
    /// event running past midnight still lists on the day it begins.
    var day: Date { Calendar.current.startOfDay(for: startsAt) }

    var hasLocation: Bool { !(location ?? "").isEmpty }
    var hasNotes: Bool { !(notes ?? "").isEmpty }
}

// One occurrence of an event inside a queried window — decoded from
// GET /v1/instances, the calendar pane's data source. A recurring event arrives
// as several of these sharing its eventId; a single event passes through as
// exactly one. Instances are derived server-side and never synced or edited:
// opening one edits the MASTER event it points at, which is why eventId is the
// only id on the wire.
struct EventInstance: Identifiable, Codable, Hashable {
    var eventId: String       // the master Event's id
    /// Same optionality and meaning as Event.calendarId; nil only on an
    /// instance synthesized from an old server's event.
    var calendarId: String?
    var title: String
    var startsAt: Date
    var endsAt: Date
    var location: String?
    var notes: String?
    /// The automation the master fires; nil when it fires nothing.
    var automation: String?
    /// True on every instance expanded from an RRULE — what the repeat glyph
    /// marks — and false on a single event's pass-through.
    var recurring: Bool
    var createdBy: String

    /// Distinct per occurrence: two instances of one event share eventId, and a
    /// list keyed on eventId alone would collapse the series onto one row.
    var id: String { "\(eventId)@\(startsAt.timeIntervalSinceReferenceDate)" }

    /// The day the instance files under; same start-keyed rule as Event.day.
    var day: Date { Calendar.current.startOfDay(for: startsAt) }

    var isUserCreated: Bool { createdBy == Event.userAuthor }
    var hasLocation: Bool { !(location ?? "").isEmpty }
    var hasNotes: Bool { !(notes ?? "").isEmpty }
    var firesAutomation: Bool { !(automation ?? "").isEmpty }

    /// The old-server fallback: a botnetd without /v1/instances still shows its
    /// events, mapped one-to-one. Such a server expands nothing and fires
    /// nothing, so recurring is false and automation nil by construction.
    static func single(_ event: Event) -> EventInstance {
        EventInstance(
            eventId: event.id, calendarId: event.calendarId, title: event.title,
            startsAt: event.startsAt, endsAt: event.endsAt,
            location: event.location, notes: event.notes,
            automation: nil, recurring: false, createdBy: event.createdBy)
    }
}

// Decoded from GET /v1/tools: the exact tools the server sends the model each
// turn. The list is heterogeneous — a function tool is
// {type:"function", function:{name, description, parameters}} (OpenAI shape),
// while a server-side tool like web search is a bare
// {type:"openrouter:web_search"} with no function object. So `type` always
// decodes and `function` is optional; a tool with no function still lists
// (showing its type) rather than failing the whole array's decode. Shown so the
// user can read precisely what the model is offered; nothing here is paraphrased.
struct ToolDefinition: Identifiable, Decodable, Hashable {
    /// "function" for a function tool, or the server tool's own tag
    /// (e.g. "openrouter:web_search").
    var type: String
    /// Present only for a function tool; nil for a server tool.
    var function: FunctionSpec?

    /// The function-calling payload, shown verbatim.
    struct FunctionSpec: Decodable, Hashable {
        var name: String
        /// Multiline prose, kept verbatim — line breaks and all.
        var description: String
        /// The parameters JSON Schema, re-indented for display. Decoded opaquely
        /// (the schema's shape will evolve), never into typed fields.
        var parametersJSON: String

        private enum Keys: String, CodingKey { case name, description, parameters }
        init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: Keys.self)
            name = try c.decode(String.self, forKey: .name)
            description = try c.decode(String.self, forKey: .description)
            parametersJSON = try c.decode(JSONValue.self, forKey: .parameters).prettyPrinted
        }
    }

    private enum CodingKeys: String, CodingKey { case type, function }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        // Default to "function" so a server that ever omits type on its function
        // tools still classifies them; a real server tool always sends its type.
        type = try c.decodeIfPresent(String.self, forKey: .type) ?? "function"
        function = try c.decodeIfPresent(FunctionSpec.self, forKey: .function)
    }

    /// Stable across the list: a function tool by its name, a server tool by its
    /// type (both unique within one /v1/tools payload).
    var id: String { function?.name ?? type }

    /// The row's heading: the function name, or a readable label derived from a
    /// server tool's type ("openrouter:web_search" → "Web search"). The raw type
    /// is shown alongside in the inspector, so this label never hides what it is.
    var displayTitle: String {
        function?.name ?? Self.humanize(type)
    }

    /// "openrouter:web_search" → "Web search": drop the namespace, split on the
    /// separators, capitalize the first word. General on purpose, so an unknown
    /// future server tool still reads as words rather than a raw tag.
    private static func humanize(_ type: String) -> String {
        let tail = type.split(whereSeparator: { $0 == ":" || $0 == "/" }).last.map(String.init) ?? type
        let words = tail.split(whereSeparator: { $0 == "_" || $0 == "-" }).joined(separator: " ")
        guard let first = words.first else { return type }
        return first.uppercased() + words.dropFirst()
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

// One automation as the bridge serves it — mirrors automationView in
// go/std/bg_services/automations/server.go, reached through botnet's mounted
// /v1/automations routes. The list row omits `runs`; the detail row carries
// the last 20, newest first. `freshness` stays a wire string (like a
// calendar's color): the closed set is ok|pending|stale|failed|never|
// unscheduled today, and Palette.freshness(_:) absorbs a value this build
// doesn't know rather than a decoder throwing on it.
struct Automation: Identifiable, Decodable, Hashable {
    var name: String
    var goal: String
    /// Repo-relative directory, for display.
    var dir: String
    /// The absolute directory path Open in Cursor launches on. Added by the
    /// bridge; nil on a service that predates it, never a real value — the
    /// open/reveal actions hide rather than guessing a path.
    var path: String?
    /// Nil for an unscheduled automation: discovered, listed, manually
    /// runnable, never auto-run.
    var schedule: AutomationSchedule?
    /// The manifest's schedule-block parse error, verbatim, when it has one;
    /// such an automation is treated as unscheduled server-side.
    var scheduleError: String?
    var freshness: String
    var lastRun: RunSummary?
    /// Detail endpoint only; nil on a list row, never an empty real value.
    var runs: [RunSummary]?

    var id: String { name }
}

// A manifest's schedule template, echoed verbatim by the API — the botnet
// calendar owns the actual future; this is what provisioned it. retryEvery /
// retryFor are the manifest's raw Go-duration strings ("2h", "30h").
struct AutomationSchedule: Decodable, Hashable {
    var rrule: String
    /// Wall-clock "HH:MM" in tz.
    var at: String
    var tz: String
    var retryEvery: String
    var retryFor: String

    /// "FREQ=MONTHLY;BYDAY=4TU · 13:05 America/New_York · retry 2h/30h" —
    /// the rule verbatim (the rrule IS the spec), same separator the calendar
    /// summaries use.
    var summary: String {
        "\(rrule) · \(at) \(tz) · retry \(retryEvery)/\(retryFor)"
    }
}

// One recorded run, without its envelope body — mirrors runSummary in the
// automations service. `started`/`finished` are RFC3339 strings on the wire,
// not Dates: `finished` is "" while the run is queued or in flight, which no
// date decoder may see. status is queued|running|ok|degraded|failed|
// needs_human|error; Palette.runStatus(_:) absorbs anything newer.
struct RunSummary: Identifiable, Decodable, Hashable {
    var id: String            // "run_" + ULID
    var automation: String
    var trigger: String       // "schedule" | "manual"
    var started: String
    var finished: String
    var exitCode: Int
    var status: String
    /// Which automation-123 form the driver reported; 0 until an envelope
    /// arrives.
    var formUsed: Int
    var escalationReason: String?

    var isFinished: Bool { !finished.isEmpty }

    var startedAt: Date? { Self.parse(started) }
    var finishedAt: Date? { Self.parse(finished) }

    /// Wall-clock run length; nil while unfinished or unparseable.
    var duration: TimeInterval? {
        guard let s = startedAt, let f = finishedAt else { return nil }
        return f.timeIntervalSince(s)
    }

    // The service writes fixed-width RFC3339 UTC (whole seconds); the plain
    // parser is enough, but tolerate a fractional stamp should that change.
    private static let iso = ISO8601DateFormatter()
    private static let isoFractional: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()
    private static func parse(_ raw: String) -> Date? {
        guard !raw.isEmpty else { return nil }
        return iso.date(from: raw) ?? isoFractional.date(from: raw)
    }
}

// GET /v1/runs/{id}: the summary flattened together with the envelope, the
// stderr tail and the service-side error. The Go side embeds runSummary, so
// the keys are flat; `summary` re-decodes them from the same container.
struct RunDetail: Decodable, Hashable {
    var summary: RunSummary
    /// The parsed result envelope. Nil when the run produced none (error
    /// runs) AND when it arrives in a shape this build can't read — the
    /// envelope's schema is the automations spec's to evolve, so an unknown
    /// shape degrades to nil rather than failing the whole run's decode.
    var envelope: RunEnvelope?
    var stderrTail: String
    var error: String

    private enum Keys: String, CodingKey { case envelope, stderrTail, error }
    init(from decoder: Decoder) throws {
        summary = try RunSummary(from: decoder)
        let c = try decoder.container(keyedBy: Keys.self)
        envelope = try? c.decodeIfPresent(RunEnvelope.self, forKey: .envelope)
        stderrTail = try c.decodeIfPresent(String.self, forKey: .stderrTail) ?? ""
        error = try c.decodeIfPresent(String.self, forKey: .error) ?? ""
    }
}

// The automation-123 result envelope a run's driver emitted — snake_case keys
// per the spec. Every key is optional (absence reads as "not reported"), but a
// key present with the wrong type throws here so RunDetail's lenient wrapper
// degrades the WHOLE envelope to nil — half an alien envelope rendered as
// truth would be worse than none.
struct RunEnvelope: Decodable, Hashable {
    var automation: String?
    var status: String?
    var formUsed: Int?
    var artifacts: [RunArtifact]
    var escalationReason: String?

    private enum Keys: String, CodingKey {
        case automation, status
        case formUsed = "form_used"
        case artifacts
        case escalationReason = "escalation_reason"
    }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: Keys.self)
        automation = try c.decodeIfPresent(String.self, forKey: .automation)
        status = try c.decodeIfPresent(String.self, forKey: .status)
        formUsed = try c.decodeIfPresent(Int.self, forKey: .formUsed)
        artifacts = try c.decodeIfPresent([RunArtifact].self, forKey: .artifacts) ?? []
        escalationReason = try c.decodeIfPresent(String.self, forKey: .escalationReason)
    }
}

/// One artifact a run touched: path, row count, newest period present.
struct RunArtifact: Decodable, Hashable {
    var path: String
    var rows: Int
    var newest: String
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
