// APIClient.swift — HTTP calls to the local botnetd server. This is the whole
// contract between the app and the state owner; matches go/botnet/server.go.

import Foundation

extension CharacterSet {
    /// `urlQueryAllowed` still permits `&` and `=`, which are exactly the
    /// characters that would let a value break out of its own query parameter.
    static let urlQueryValueAllowed = CharacterSet.urlQueryAllowed
        .subtracting(CharacterSet(charactersIn: "&=+?"))
}

struct APIClient {
    var base = ProcessInfo.processInfo.environment["BOTNET_API"].flatMap(URL.init(string:))
        ?? URL(string: "http://127.0.0.1:8730")!

    struct ServerError: LocalizedError {
        var message: String
        var status: Int
        /// The raw failure body. A failed send returns the transcript alongside
        /// the error so the stranded turn can be rendered without a refetch.
        var body: Data
        var errorDescription: String? { message }
    }

    /// Raised when the server refuses a send because this bot already has a
    /// reply in flight. It hands back the turn holding the bot, which is the one
    /// worth waiting on rather than guessing about.
    struct BusyError: LocalizedError {
        var inFlight: Message
        var errorDescription: String? { "This bot is still replying. Wait for it to finish." }
    }

    // Go marshals time.Time with fractional seconds ("…T15:36:13.816732Z") and
    // its zero value as year 1 ("0001-01-01T00:00:00Z"). Plain .iso8601 rejects
    // the fractional form on the macOS 14 deployment target, so try both forms
    // explicitly; the year-1 sentinel must decode (not throw) for
    // Date.nilIfServerZero to see it.
    private static let isoFractional: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()
    private static let isoPlain = ISO8601DateFormatter()

    /// The RFC3339 form the server parses on the way in. Whole seconds in UTC:
    /// a calendar time is chosen by the user to the minute, and the fractional
    /// part would only be noise in a query string.
    static func wireTime(_ date: Date) -> String { isoPlain.string(from: date) }

    var decoder: JSONDecoder {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .custom { decoder in
            let container = try decoder.singleValueContainer()
            let raw = try container.decode(String.self)
            guard let date = Self.isoFractional.date(from: raw) ?? Self.isoPlain.date(from: raw) else {
                throw DecodingError.dataCorruptedError(
                    in: container, debugDescription: "unrecognized server timestamp: \(raw)")
            }
            return date
        }
        return d
    }

    func hasKey() async throws -> Bool {
        let r: [String: Bool] = try await get("/v1/config")
        return r["hasKey"] ?? false
    }

    func setKey(_ key: String) async throws {
        let _: [String: Bool] = try await send("/v1/config", method: "POST", body: ["openRouterKey": key])
    }

    func listBots() async throws -> [Bot] {
        try await get("/v1/bots")
    }

    func listModels() async throws -> [ModelOption] {
        // server returns {name, id}; ModelOption decodes those two keys
        try await get("/v1/models")
    }

    /// 404 on servers that predate the route; callers go through
    /// `isUnimplemented` and hide the feature rather than erroring.
    func listTools() async throws -> [ToolDefinition] {
        try await get("/v1/tools")
    }

    // MARK: calendar
    //
    // The calendar is the first service with its own collection. Events are not
    // versioned, so an update is a plain PATCH of whatever fields changed;
    // 404 on all four routes means a botnetd that predates the service, which
    // callers hide rather than report.

    /// Ascending by start. The optional window filters by overlap server-side
    /// (endsAt > from AND startsAt < to); omitting both asks for everything.
    func listEvents(from: Date? = nil, to: Date? = nil) async throws -> [Event] {
        var query: [String] = []
        if let from { query.append("from=" + escaped(Self.wireTime(from))) }
        if let to { query.append("to=" + escaped(Self.wireTime(to))) }
        let suffix = query.isEmpty ? "" : "?" + query.joined(separator: "&")
        return try await get("/v1/events" + suffix)
    }

    /// createdBy is the server's call, not ours: an event posted here is "user".
    /// A nil calendarId is omitted, not sent as "": the server then files the
    /// event under "Personal" (creating it if needed), which is the contract's
    /// default and also what an older client's create already means.
    func createEvent(title: String, startsAt: Date, endsAt: Date,
                     location: String, notes: String, calendarId: String? = nil) async throws -> Event {
        var body = [
            "title": title,
            "startsAt": Self.wireTime(startsAt),
            "endsAt": Self.wireTime(endsAt),
        ]
        // Absent rather than "" so a create matches the wire shape a bot's tool
        // call produces; the server treats both as unset anyway.
        if !location.isEmpty { body["location"] = location }
        if !notes.isEmpty { body["notes"] = notes }
        if let calendarId { body["calendarId"] = calendarId }
        return try await send("/v1/events", method: "POST", body: body)
    }

    /// `fields` is any subset of the create body; an empty string clears a field.
    func updateEvent(_ id: String, fields: [String: String]) async throws -> Event {
        try await send("/v1/events/\(id)", method: "PATCH", body: fields)
    }

    func deleteEvent(_ id: String) async throws {
        _ = try await raw("/v1/events/\(id)", method: "DELETE", body: nil)
    }

    /// Expanded instances of every event overlapping the window, ascending by
    /// start: single events pass through, recurring events expand server-side.
    /// Both bounds are required by the contract (window cap 400 days), so
    /// neither is optional here. 404 means a botnetd that predates instances;
    /// callers fall back to the wholesale events list.
    func listInstances(from: Date, to: Date) async throws -> [EventInstance] {
        try await get("/v1/instances?from=" + escaped(Self.wireTime(from))
                      + "&to=" + escaped(Self.wireTime(to)))
    }

    // MARK: calendars (the named collections events file under)
    //
    // Same era and same rules as the calendar-tool routes above: last-write-wins,
    // and a 404 on any of them means a botnetd that predates multiple calendars,
    // which callers read through `isUnimplemented` and hide rather than report.

    /// Ascending by createdAt, as the server orders them. Never null on the
    /// wire — an empty list is `[]`, and a valid answer.
    func listCalendars() async throws -> [EventCalendar] {
        try await get("/v1/calendars")
    }

    /// A nil color is omitted: the server then cycles its enum by calendar
    /// count, which spreads the colors without the client hardcoding the order.
    func createCalendar(name: String, color: String? = nil) async throws -> EventCalendar {
        var body = ["name": name]
        if let color { body["color"] = color }
        return try await send("/v1/calendars", method: "POST", body: body)
    }

    /// `fields` is any subset of {name, color}, PATCHed partially like an event.
    func updateCalendar(_ id: String, fields: [String: String]) async throws -> EventCalendar {
        try await send("/v1/calendars/\(id)", method: "PATCH", body: fields)
    }

    /// CASCADES server-side: the calendar's events are deleted with it. The UI
    /// confirms before calling; the wire call itself is unconditional.
    func deleteCalendar(_ id: String) async throws {
        _ = try await raw("/v1/calendars/\(id)", method: "DELETE", body: nil)
    }

    private func escaped(_ value: String) -> String {
        value.addingPercentEncoding(withAllowedCharacters: .urlQueryValueAllowed) ?? value
    }

    func createBot(displayName: String, systemPrompt: String, model: String) async throws -> Bot {
        try await send("/v1/bots", method: "POST", body: [
            "displayName": displayName, "systemPrompt": systemPrompt, "model": model,
        ])
    }

    func deleteBot(_ id: String) async throws {
        _ = try await raw("/v1/bots/\(id)", method: "DELETE", body: nil)
    }

    func messages(_ botID: String) async throws -> [Message] {
        try await get("/v1/bots/\(botID)/messages")
    }

    func patchBot(_ id: String, fields: [String: String]) async throws -> Bot {
        try await send("/v1/bots/\(id)", method: "PATCH", body: fields)
    }

    func segments(_ botID: String) async throws -> [Segment] {
        try await get("/v1/bots/\(botID)/segments")
    }

    func compact(_ botID: String) async throws -> [Segment] {
        try decode(await raw("/v1/bots/\(botID)/compact", method: "POST", body: nil),
                   from: "/v1/bots/\(botID)/compact")
    }

    func markRead(_ botID: String) async throws -> Bot {
        try decode(await raw("/v1/bots/\(botID)/read", method: "POST", body: nil),
                   from: "/v1/bots/\(botID)/read")
    }

    func retry(_ messageID: String, on botID: String) async throws -> Message {
        try decode(await raw("/v1/bots/\(botID)/messages/\(messageID)/retry", method: "POST", body: nil),
                   from: "/v1/bots/\(botID)/messages/\(messageID)/retry")
    }

    func message(_ id: String) async throws -> Message {
        try await get("/v1/messages/\(id)")
    }

    /// Only what follows `cursor`. A 404 means the cursor is unknown to this bot
    /// and the caller must resync rather than keep polling a dead id.
    func messages(_ botID: String, after cursor: String) async throws -> [Message] {
        return try await get("/v1/bots/\(botID)/messages?after=\(escaped(cursor))")
    }

    /// True when the server has no such route, which is how this build tells an
    /// older botnetd apart from a real failure.
    static func isUnimplemented(_ error: Error) -> Bool {
        (error as? ServerError)?.status == 404
    }

    /// Posts a turn and returns as soon as the server has persisted it, well
    /// before the model replies. The returned Message carries the real id and
    /// `awaiting` status; the reply is collected separately.
    func send(_ content: String, to botID: String) async throws -> Message {
        do {
            let data = try JSONSerialization.data(withJSONObject: ["content": content])
            return try decode(await raw("/v1/bots/\(botID)/messages", method: "POST", body: data),
                              from: "POST /v1/bots/\(botID)/messages")
        } catch let error as ServerError where error.status == 409 {
            guard let busy = try? decoder.decode(BusyBody.self, from: error.body) else { throw error }
            throw BusyError(inFlight: busy.message)
        }
    }

    private struct BusyBody: Decodable {
        var error: String
        var message: Message
    }

    // MARK: transport

    private func get<T: Decodable>(_ path: String) async throws -> T {
        try decode(await raw(path, method: "GET", body: nil), from: path)
    }

    private func send<T: Decodable>(_ path: String, method: String, body: [String: String]) async throws -> T {
        let data = try JSONSerialization.data(withJSONObject: body)
        return try decode(await raw(path, method: method, body: data), from: path)
    }

    // A bare DecodingError alerts as "The data couldn't be read…", naming
    // neither the endpoint nor the body, which made a stale-client shape
    // mismatch undiagnosable from the alert alone. Keep both.
    func decode<T: Decodable>(_ data: Data, from path: String) throws -> T {
        do {
            return try decoder.decode(T.self, from: data)
        } catch let error as DecodingError {
            let dump = FileManager.default.temporaryDirectory
                .appendingPathComponent("botnet-decode-failure.json")
            try? data.write(to: dump)
            throw ServerError(
                message: "\(path) returned a shape this build can't decode (\(Self.detail(error))) — body saved to \(dump.path)",
                status: 0, body: data)
        }
    }

    private static func detail(_ error: DecodingError) -> String {
        switch error {
        case .typeMismatch(_, let ctx), .valueNotFound(_, let ctx), .dataCorrupted(let ctx):
            return ctx.debugDescription
        case .keyNotFound(let key, _):
            return "missing key \(key.stringValue)"
        @unknown default:
            return String(describing: error)
        }
    }

    // appendingPathComponent percent-encodes the whole string, so a path
    // carrying a query ("...?after=x") turns into a literal path segment and the
    // request 404s. Compose textually instead; callers encode their own values.
    private func url(_ path: String) throws -> URL {
        guard let url = URL(string: base.absoluteString + path) else {
            throw ServerError(message: "bad url for \(path)", status: 0, body: Data())
        }
        return url
    }

    @discardableResult
    private func raw(_ path: String, method: String, body: Data?) async throws -> Data {
        var req = try URLRequest(url: url(path))
        req.httpMethod = method
        if let body {
            req.httpBody = body
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let (data, resp) = try await URLSession.shared.data(for: req)
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        guard (200..<300).contains(code) else {
            if let err = try? JSONDecoder().decode([String: String].self, from: data), let m = err["error"] {
                throw ServerError(message: m, status: code, body: data)
            }
            throw ServerError(message: "server error (\(code))", status: code, body: data)
        }
        return data
    }
}
