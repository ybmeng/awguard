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

// MARK: - Projects
//
// A project is what work is ABOUT: a goal plus typed, dated facts. Health and
// nextDue are DERIVED server-side from the facts and never stored, the same way
// an automation's freshness derives from its runs — so nothing here computes
// them, and a client that disagreed with the server would just be wrong.

// One project — mirrors go/botnet/schema.go's Project. `health` stays the wire
// string (like a calendar's color and an automation's freshness): the closed set
// is overdue|blocked|due_soon|ok|unknown today, and Palette.health(_:) absorbs a
// value this build doesn't know rather than a decoder throwing on it.
struct Project: Identifiable, Decodable, Hashable {
    var id: String            // "prj_" + ULID
    var name: String
    var goal: String          // free text, may be empty
    /// The bot id that created it, or `Event.userAuthor` for one made in the UI.
    var createdBy: String
    var createdAt: Date
    var updatedAt: Date
    /// The parent project's id, or nil at the top level. The wire spells "top
    /// level" two ways — the key omitted (Go's omitempty) and "" — and both
    /// arrive here as nil, because the tree has exactly one notion of a root
    /// and two spellings of it would split it in half.
    var parentId: String?
    /// Derived at read time from the facts of this project AND its whole
    /// subtree; never patched, never sent back.
    var health: String
    /// The coarse three-step reading of `health` ("S0" | "S1" | "S2"), derived
    /// from the same rolled-up health. A wire string like `health` is: an "S3"
    /// from a newer server has to render, not throw. "" means a server that
    /// predates severity, which is why `hasSeverity` exists.
    var severity: String
    /// The nearest upcoming due instant across the undone deadline and
    /// recurring facts, this project's and its subtree's. Nil when nothing is
    /// pending.
    var nextDue: Date?
    /// This project's OWN facts — a parent's count does not include its
    /// children's, which is what makes "3 facts" on a parent readable.
    var factCount: Int
    /// DIRECT children. The server's own count; the sidebar's disclosure keys
    /// off the tree it actually built, so this is for prose (the delete
    /// confirmation) rather than for layout.
    var childCount: Int

    var isUserCreated: Bool { createdBy == Event.userAuthor }
    var hasGoal: Bool { !goal.isEmpty }
    var isTopLevel: Bool { parentId == nil }
    var hasChildren: Bool { childCount > 0 }
    var hasSeverity: Bool { !severity.isEmpty }

    /// "due soon" — the wire value read as words. An unknown future value comes
    /// through verbatim rather than being hidden behind a guess.
    var healthLabel: String { health.replacingOccurrences(of: "_", with: " ") }

    /// "in 12d" / "today" / "overdue 3d", or nil when nothing is due.
    var nextDueText: String? { DueText.relative(nextDue) }

    /// "overdue 13d" / "due soon · in 18d" / "ok · in 193d" / "unknown". When
    /// the relative reading already carries the health word it stands alone:
    /// "overdue · overdue 13d" says the same thing twice.
    var healthText: String {
        guard let due = nextDueText else { return healthLabel }
        guard !due.hasPrefix(healthLabel) else { return due }
        return "\(healthLabel) · \(due)"
    }

    /// "S0 · overdue 13d" — the severity first, because it is the thing that
    /// sorts the list, then the health reading it coarsens. A server that
    /// derives no severity says only the health part rather than an empty
    /// prefix and a stray separator.
    var severityText: String {
        hasSeverity ? "\(severity) · \(healthText)" : healthText
    }

    private enum Keys: String, CodingKey {
        case id, name, goal, parentId, createdBy, createdAt, updatedAt
        case health, severity, nextDue, factCount, childCount
    }
    // Every derived key is read leniently: Go's omitempty drops exactly the zero
    // value, so an absent factCount IS 0 and an absent goal IS "". nextDue goes
    // through nilIfServerZero as well, because a server marshalling an unset
    // time.Time rather than a *time.Time sends year 1, not null, and a year-1
    // date on a sidebar row would read as wildly overdue.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: Keys.self)
        id = try c.decode(String.self, forKey: .id)
        name = try c.decode(String.self, forKey: .name)
        goal = try c.decodeIfPresent(String.self, forKey: .goal) ?? ""
        // "" is the wire's other way of saying top level; it collapses to nil
        // here so nothing downstream has to test for both.
        let parent = try c.decodeIfPresent(String.self, forKey: .parentId) ?? ""
        parentId = parent.isEmpty ? nil : parent
        createdBy = try c.decodeIfPresent(String.self, forKey: .createdBy) ?? ""
        createdAt = try c.decode(Date.self, forKey: .createdAt)
        updatedAt = try c.decode(Date.self, forKey: .updatedAt)
        health = try c.decodeIfPresent(String.self, forKey: .health) ?? ""
        severity = try c.decodeIfPresent(String.self, forKey: .severity) ?? ""
        nextDue = try c.decodeIfPresent(Date.self, forKey: .nextDue)?.nilIfServerZero
        factCount = try c.decodeIfPresent(Int.self, forKey: .factCount) ?? 0
        childCount = try c.decodeIfPresent(Int.self, forKey: .childCount) ?? 0
    }
}

// One typed fact on a project — mirrors go/botnet/schema.go's Fact. Which
// optional keys ride along depends on `kind`: due on deadline/recurring, rrule
// and tz on recurring, blocker on milestone, body anywhere. `kind` stays a wire
// string for the same reason `health` does.
struct ProjectFact: Identifiable, Decodable, Hashable {
    var id: String            // "fct_" + ULID
    var projectId: String
    var kind: String
    var title: String
    /// The deadline's instant, or a recurring fact's FIRST occurrence. Absent
    /// (and year-1) on a milestone or a note.
    var due: Date?
    /// Days before `due` that count as due_soon. Meaningless without a due.
    var leadDays: Int
    var rrule: String?
    var tz: String?
    /// Only a deadline or a milestone can be done; a done fact affects neither
    /// health nor nextDue.
    var done: Bool
    /// Milestone only. Non-empty means the milestone waits on a human.
    var blocker: String?
    var body: String?
    /// The projected calendar event, when the server keeps one for this fact.
    /// Nil for milestones, notes, done facts, and a server that predates the
    /// projection.
    var eventId: String?
    var createdBy: String
    var createdAt: Date
    var updatedAt: Date

    var isUserCreated: Bool { createdBy == Event.userAuthor }
    var isBlocked: Bool { !(blocker ?? "").isEmpty }
    var isRecurring: Bool { !(rrule ?? "").isEmpty }
    var hasBody: Bool { !(body ?? "").isEmpty }

    /// Whether the row draws a done toggle. Keyed off the typed kind, so a kind
    /// this build doesn't know gets no toggle rather than a PATCH the server
    /// would reject.
    var isCompletable: Bool { FactKind(rawValue: kind)?.isCompletable ?? false }

    /// "in 12d" / "overdue 3d", or nil for a fact with no due date.
    var dueText: String? { DueText.relative(due) }

    private enum Keys: String, CodingKey {
        case id, projectId, kind, title, due, leadDays, rrule, tz, done
        case blocker, body, eventId, createdBy, createdAt, updatedAt
    }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: Keys.self)
        id = try c.decode(String.self, forKey: .id)
        projectId = try c.decodeIfPresent(String.self, forKey: .projectId) ?? ""
        kind = try c.decodeIfPresent(String.self, forKey: .kind) ?? ""
        title = try c.decode(String.self, forKey: .title)
        due = try c.decodeIfPresent(Date.self, forKey: .due)?.nilIfServerZero
        leadDays = try c.decodeIfPresent(Int.self, forKey: .leadDays) ?? 0
        rrule = try c.decodeIfPresent(String.self, forKey: .rrule)
        tz = try c.decodeIfPresent(String.self, forKey: .tz)
        done = try c.decodeIfPresent(Bool.self, forKey: .done) ?? false
        blocker = try c.decodeIfPresent(String.self, forKey: .blocker)
        body = try c.decodeIfPresent(String.self, forKey: .body)
        eventId = try c.decodeIfPresent(String.self, forKey: .eventId)
        createdBy = try c.decodeIfPresent(String.self, forKey: .createdBy) ?? ""
        createdAt = try c.decode(Date.self, forKey: .createdAt)
        updatedAt = try c.decode(Date.self, forKey: .updatedAt)
    }
}

/// GET /v1/projects/{id}: the project and its facts, already sorted
/// urgency-first by the server. The pane renders them in exactly that order —
/// the sort is the server's contract, and re-sorting here would be a second
/// opinion nothing keeps in step.
struct ProjectDetail: Decodable, Hashable {
    var project: Project
    /// A project with no facts sends `facts` as null (Go's nil slice), which is
    /// an empty list, not a missing one.
    var facts: [ProjectFact]
    /// The DIRECT children, with their own derived fields, in the same sort the
    /// list uses. Null (no children) and absent (a server that predates the
    /// hierarchy) are both an empty list, exactly like `facts`.
    var children: [Project]

    private enum Keys: String, CodingKey { case project, facts, children }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: Keys.self)
        project = try c.decode(Project.self, forKey: .project)
        facts = try c.decodeIfPresent([ProjectFact].self, forKey: .facts) ?? []
        children = try c.decodeIfPresent([Project].self, forKey: .children) ?? []
    }
}

/// The project hierarchy, built ONCE from the flat list the server sends.
///
/// Every view reads its shape from here and no view walks `parentId` itself:
/// the sidebar's rows, the pane's children strip and the Edit sheet's parent
/// picker are three readings of one structure, and three separate walks would
/// disagree the first time an edge case (an orphan, a rename, a cycle) showed
/// up in exactly one of them.
///
/// Two rules the server guarantees but this cannot assume, because the client
/// also renders a list mid-write and a list from an older server:
/// a project whose parent is not in the list is an ORPHAN and renders as a
/// root — dropping it would hide real work — and a parent chain that loops is
/// walked at most once per project rather than forever.
struct ProjectTree {
    /// The flat list, in the server's own order (severity/health precedence,
    /// then nextDue, then name). Sibling order within the tree is exactly this
    /// order; nothing here re-sorts.
    let all: [Project]
    /// Top-level projects and orphans, in list order.
    let roots: [Project]

    private let byID: [String: Project]
    private let byParent: [String: [Project]]

    init(_ projects: [Project]) {
        all = projects
        var byID: [String: Project] = [:]
        for p in projects { byID[p.id] = p }
        var byParent: [String: [Project]] = [:]
        var roots: [Project] = []
        for p in projects {
            // A parent that isn't in this list can't be rendered as a parent,
            // so its child is a root here rather than an invisible node.
            if let parent = p.parentId, parent != p.id, byID[parent] != nil {
                byParent[parent, default: []].append(p)
            } else {
                roots.append(p)
            }
        }
        self.byID = byID
        self.byParent = byParent
        self.roots = roots
    }

    func project(_ id: String) -> Project? { byID[id] }

    /// Direct children, in list order.
    func children(of id: String) -> [Project] { byParent[id] ?? [] }

    func hasChildren(_ id: String) -> Bool { byParent[id] != nil }

    /// How far in the row indents: 0 for a root. A looping parent chain stops
    /// at the first repeat rather than hanging the UI.
    func depth(of id: String) -> Int {
        var depth = 0
        var seen: Set<String> = [id]
        var cursor = byID[id]?.parentId
        while let parent = cursor, !seen.contains(parent), let node = byID[parent] {
            seen.insert(parent)
            depth += 1
            cursor = node.parentId
        }
        return depth
    }

    /// The project AND all its descendants, preorder. Includes self, because
    /// every caller wants it: the parent picker excludes this whole set (you
    /// cannot be your own parent or your descendant's), and the delete
    /// confirmation counts it.
    func subtree(of id: String) -> [Project] {
        guard let root = byID[id] else { return [] }
        var out: [Project] = []
        var stack = [root]
        var seen = Set<String>()
        while let node = stack.popLast() {
            guard seen.insert(node.id).inserted else { continue }
            out.append(node)
            stack.append(contentsOf: children(of: node.id).reversed())
        }
        return out
    }

    /// One rendered sidebar line: which project, how far in, whether it can
    /// disclose and whether it currently is.
    struct Row: Identifiable, Hashable {
        let project: Project
        let depth: Int
        let hasChildren: Bool
        let expanded: Bool

        var id: String { project.id }
    }

    /// The rows the sidebar draws, in order.
    ///
    /// With no query: preorder from the roots, descending only into ids in
    /// `expanded`. With a query: the matching projects PLUS the ancestors
    /// needed to reach them, every such ancestor force-revealed — a match two
    /// levels down is useless if its parent is collapsed, and the persisted
    /// expansion is left untouched so it comes back when the search clears.
    func rows(expanded: Set<String>, query: String = "") -> [Row] {
        let needle = query.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !needle.isEmpty else { return rows(from: roots, expanded: expanded, visible: nil) }
        var visible = Set<String>()
        for p in all where p.name.range(of: needle, options: .caseInsensitive) != nil {
            for ancestor in ancestry(of: p.id) { visible.insert(ancestor) }
        }
        guard !visible.isEmpty else { return [] }
        return rows(from: roots.filter { visible.contains($0.id) },
                    expanded: visible, visible: visible)
    }

    /// Every project the given one could be moved under: the whole tree minus
    /// its own subtree, in render order with depth. Excluding the subtree is
    /// what makes a cycle unofferable rather than a 400 the user has to read —
    /// the server still refuses one, but the picker never proposes it.
    ///
    /// Here rather than in the sheet so it is provable without a view, and so
    /// the one place that knows the shape is the one place that answers this.
    func parentCandidates(for id: String) -> [Row] {
        let excluded = Set(subtree(of: id).map(\.id))
        return rows(expanded: Set(all.map(\.id))).filter { !excluded.contains($0.project.id) }
    }

    /// A project and every ancestor above it, self first. Cycle-guarded like
    /// `depth(of:)`.
    private func ancestry(of id: String) -> [String] {
        var out: [String] = []
        var seen = Set<String>()
        var cursor: String? = id
        while let current = cursor, seen.insert(current).inserted, let node = byID[current] {
            out.append(current)
            cursor = node.parentId
        }
        return out
    }

    private func rows(from level: [Project], expanded: Set<String>,
                      visible: Set<String>?, depth: Int = 0) -> [Row] {
        var out: [Row] = []
        for project in level {
            let kids = children(of: project.id).filter { visible?.contains($0.id) ?? true }
            let open = expanded.contains(project.id)
            out.append(Row(project: project, depth: depth,
                           hasChildren: !kids.isEmpty, expanded: open && !kids.isEmpty))
            if open, !kids.isEmpty {
                out += rows(from: kids, expanded: expanded, visible: visible, depth: depth + 1)
            }
        }
        return out
    }
}

/// The four fact kinds this build can author. Deliberately a Swift enum over
/// the wire string: `fields` is the ONE table saying which inputs a kind takes,
/// so the Add Fact sheet is driven by it rather than by four branches, and the
/// contract's validation rules live in one readable place. A wire `kind` outside
/// this set still decodes and renders — it just can't be authored here.
enum FactKind: String, CaseIterable, Identifiable {
    case deadline, recurring, milestone, note

    var id: String { rawValue }

    var title: String { rawValue.capitalized }

    /// The row's leading glyph. A kind this build doesn't know falls back to a
    /// neutral mark rather than borrowing another kind's meaning.
    var symbol: String {
        switch self {
        case .deadline: return "calendar.badge.exclamationmark"
        case .recurring: return "arrow.triangle.2.circlepath"
        case .milestone: return "flag"
        case .note: return "text.alignleft"
        }
    }

    /// Which inputs the Add Fact sheet shows, mirroring the server's write
    /// boundary: deadline requires a due, recurring requires due+rrule+tz,
    /// milestone/note must carry no due at all, and only a milestone may carry
    /// a blocker. Sending a field outside this set is a 400, so the sheet
    /// cannot offer one.
    var fields: Set<FactField> {
        switch self {
        case .deadline: return [.due, .leadDays, .body]
        case .recurring: return [.due, .leadDays, .rrule, .tz, .body]
        case .milestone: return [.blocker, .body]
        case .note: return [.body]
        }
    }

    /// Only a deadline or a milestone can be marked done; the server rejects
    /// `done` on the other two.
    var isCompletable: Bool { self == .deadline || self == .milestone }

    /// The glyph for a wire kind, known or not.
    static func symbol(for wire: String) -> String {
        FactKind(rawValue: wire)?.symbol ?? "circle"
    }
}

/// One input the Add Fact sheet can show. Its only job is to be the key in
/// FactKind.fields; the sheet reads that set and nothing else decides layout.
enum FactField: Hashable {
    case due, leadDays, rrule, tz, blocker, body
}

/// How a due date reads on a row. One helper because the sidebar and the pane
/// must phrase the same instant the same way; two copies would drift the moment
/// one of them rounded differently.
enum DueText {
    /// "today" / "in 12d" / "overdue 3d", nil when there is no date. Whole
    /// LOCAL days, so a due date reads the way a calendar reads it rather than
    /// flipping at an arbitrary hour.
    static func relative(_ due: Date?, now: Date = Date()) -> String? {
        guard let due else { return nil }
        let calendar = Calendar.current
        let days = calendar.dateComponents(
            [.day],
            from: calendar.startOfDay(for: now),
            to: calendar.startOfDay(for: due)
        ).day ?? 0
        if days == 0 { return "today" }
        if days > 0 { return "in \(days)d" }
        return "overdue \(-days)d"
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
