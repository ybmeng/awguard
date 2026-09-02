// decode-check.swift — proves APIClient's decoder against the timestamp shapes
// botnetd actually emits: fractional seconds (Go time.Now()), plain seconds,
// and the zero-time sentinel. Compiled against the real Sources/ files by
// dev/decode-check.sh; also decodes live GETs so the transport path is covered.

import Foundation

@main
struct DecodeCheck {
    static var failed = false

    static func check(_ name: String, _ body: () async throws -> Void) async {
        do {
            try await body()
            print("PASS \(name)")
        } catch {
            failed = true
            print("FAIL \(name): \(error)")
        }
    }

    static func main() async {
        let api = APIClient()
        let decoder = api.decoder

        // Exact 202 body captured from the live server on 2026-08-29.
        let fractional = Data("""
        {"id":"msg_01M172M37R7CXGZAAQGF5ZTHY1","botId":"bot_01M172K2T51MJDV0B263GQ1BFA","segmentId":"seg_01M172K2T8J3WYSK49A453JM81","role":"user","content":"debug: reply with exactly ok","sentAt":"2026-08-29T15:36:13.816732Z","status":"awaiting","error":""}
        """.utf8)
        await check("fractional-seconds sentAt (captured 202 body)") {
            _ = try decoder.decode(Message.self, from: fractional)
        }

        // The same message truncated to whole seconds still has to parse.
        let plain = Data("""
        {"id":"msg_01M172M37R7CXGZAAQGF5ZTHY1","botId":"bot_01M172K2T51MJDV0B263GQ1BFA","segmentId":"seg_01M172K2T8J3WYSK49A453JM81","role":"user","content":"debug: reply with exactly ok","sentAt":"2026-08-29T15:36:13Z","status":"awaiting","error":""}
        """.utf8)
        await check("second-precision sentAt") {
            _ = try decoder.decode(Message.self, from: plain)
        }

        // Go marshals zero time.Time as year 1; nilIfServerZero depends on it
        // decoding rather than throwing.
        let sentinel = Data("""
        {"id":"msg_01M172M37R7CXGZAAQGF5ZTHY1","botId":"bot_01M172K2T51MJDV0B263GQ1BFA","segmentId":null,"role":"user","content":"x","sentAt":"0001-01-01T00:00:00Z","status":"sent","error":""}
        """.utf8)
        await check("zero-time sentinel decodes and isServerZero") {
            let m = try decoder.decode(Message.self, from: sentinel)
            guard m.sentAt.isServerZero else {
                throw NSError(domain: "decode-check", code: 1, userInfo: [
                    NSLocalizedDescriptionKey: "sentinel decoded but isServerZero is false: \(m.sentAt)",
                ])
            }
        }

        // The server does not zero-pad fractions, so digit counts vary; this
        // readAt is verbatim from the live server (5 digits, not 6).
        let bot = Data("""
        {"id":"bot_01M16K3W6TZ0EHQFPKZ490BDX2","displayName":"hihi","createdAt":"2026-08-29T11:05:13.690398Z","systemPrompt":"","model":"openrouter/deepseek/deepseek-v4-flash-0731","lastMessageAt":"2026-08-29T14:57:22.589727Z","lastMessageText":"ok","readAt":"2026-08-29T11:05:22.48899Z","modelValid":true}
        """.utf8)
        await check("variable-length fractions in Bot (captured /v1/bots entry)") {
            _ = try decoder.decode(Bot.self, from: bot)
        }

        // Memory landed after the first server release: present it must decode
        // (the reply-settle bot refetch is what updates the memory panel), and
        // the older fixture above must keep decoding with memory nil.
        let botWithMemory = Data("""
        {"id":"bot_01M16K3W6TZ0EHQFPKZ490BDX2","displayName":"hihi","createdAt":"2026-08-29T11:05:13.690398Z","systemPrompt":"","model":"openrouter/deepseek/deepseek-v4-flash-0731","memory":"Birthday: March 3.","lastMessageAt":"2026-08-29T14:57:22.589727Z","lastMessageText":"ok","readAt":"2026-08-29T11:05:22.48899Z","modelValid":true}
        """.utf8)
        await check("Bot.memory decodes when present, nil when absent") {
            let new = try decoder.decode(Bot.self, from: botWithMemory)
            guard new.memory == "Birthday: March 3." else {
                throw NSError(domain: "decode-check", code: 4, userInfo: [
                    NSLocalizedDescriptionKey: "memory decoded as \(String(describing: new.memory))",
                ])
            }
            let old = try decoder.decode(Bot.self, from: bot)
            guard old.memory == nil else {
                throw NSError(domain: "decode-check", code: 5, userInfo: [
                    NSLocalizedDescriptionKey: "absent memory decoded as \(String(describing: old.memory))",
                ])
            }
        }

        // Citations landed with web search: a bot reply that used the tool
        // carries them, the ordinary reply omits the key. Present, they decode in
        // order with snippet and indices; an empty title falls back to the host.
        // Absent (every fixture above has no citations key) they must stay nil.
        let replyWithCitations = Data("""
        {"id":"msg_01M9CITEAAAAAAAAAAAAAAAAA1","botId":"bot_01M172K2T51MJDV0B263GQ1BFA","segmentId":null,"role":"bot","content":"Here is what I found.","sentAt":"2026-08-30T10:00:00Z","status":"sent","error":"","citations":[{"url":"https://apnews.com/article/markets","title":"Markets steady","snippet":"an excerpt","startIndex":10,"endIndex":42},{"url":"https://en.wikipedia.org/wiki/RAG","title":""}]}
        """.utf8)
        await check("Message.citations decodes in order when present, nil when absent") {
            let withCites = try decoder.decode(Message.self, from: replyWithCitations)
            guard withCites.citations?.count == 2,
                  withCites.citations?[0].url == "https://apnews.com/article/markets",
                  withCites.citations?[0].title == "Markets steady",
                  withCites.citations?[0].snippet == "an excerpt",
                  withCites.citations?[0].startIndex == 10,
                  withCites.citations?[0].endIndex == 42,
                  // Empty title must fall back to the host for the sources row.
                  withCites.citations?[1].displayTitle == "en.wikipedia.org"
            else {
                throw NSError(domain: "decode-check", code: 9, userInfo: [
                    NSLocalizedDescriptionKey: "citations decoded wrong: \(String(describing: withCites.citations))",
                ])
            }
            let noCites = try decoder.decode(Message.self, from: plain)
            guard noCites.citations == nil else {
                throw NSError(domain: "decode-check", code: 10, userInfo: [
                    NSLocalizedDescriptionKey: "absent citations decoded as \(String(describing: noCites.citations))",
                ])
            }
        }

        // Tool calls landed with the client-side search router: a reply that ran
        // a tool carries an ordered toolCalls array (a web_search with backend +
        // structured results, a memory call with its command + result string);
        // the ordinary reply omits the key and it must stay nil. web_search
        // `results` map to the same Citation shape the Sources row uses.
        let replyWithToolCalls = Data("""
        {"id":"msg_01M9TOOLCALLSAAAAAAAAAAAA1","botId":"bot_01M172K2T51MJDV0B263GQ1BFA","segmentId":null,"role":"bot","content":"Done.","sentAt":"2026-08-30T10:00:00Z","status":"sent","error":"","citations":[{"url":"https://apnews.com/x","title":"AP"}],"toolCalls":[{"name":"web_search","arguments":"{\\"query\\":\\"gdp 2026\\"}","result":"1. AP — https://apnews.com/x","backend":"exa","results":[{"url":"https://apnews.com/x","title":"AP"}],"at":"2026-08-30T09:59:58Z"},{"name":"memory","arguments":"{\\"command\\":\\"replace\\",\\"content\\":\\"x\\"}","result":"memory saved","at":"2026-08-30T09:59:59Z"}]}
        """.utf8)
        await check("Message.toolCalls decodes in order when present, nil when absent") {
            let withCalls = try decoder.decode(Message.self, from: replyWithToolCalls)
            guard withCalls.toolCalls?.count == 2,
                  let search = withCalls.toolCalls?[0], let mem = withCalls.toolCalls?[1],
                  search.name == "web_search", search.query == "gdp 2026",
                  search.backend == "exa", search.citations.count == 1,
                  search.citations.first?.displayTitle == "AP",
                  mem.name == "memory", mem.command == "replace",
                  mem.result == "memory saved", mem.results == nil
            else {
                throw NSError(domain: "decode-check", code: 12, userInfo: [
                    NSLocalizedDescriptionKey: "toolCalls decoded wrong: \(String(describing: withCalls.toolCalls))",
                ])
            }
            let noCalls = try decoder.decode(Message.self, from: plain)
            guard noCalls.toolCalls == nil else {
                throw NSError(domain: "decode-check", code: 13, userInfo: [
                    NSLocalizedDescriptionKey: "absent toolCalls decoded as \(String(describing: noCalls.toolCalls))",
                ])
            }
        }

        // GET /v1/tools carries what the model is literally told: the multiline
        // description must survive verbatim and the parameters schema — decoded
        // opaquely, since its shape will evolve — must re-indent to real JSON.
        let tools = Data("""
        [{"type":"function","function":{"name":"memory","description":"Replace your memory document.\\n\\nWrite the full document each time; it is not appended.","parameters":{"type":"object","properties":{"content":{"type":"string","description":"the new memory document"}},"required":["content"],"maxLength":3}}}]
        """.utf8)
        await check("ToolDefinition: verbatim description, opaque pretty schema") {
            let list = try decoder.decode([ToolDefinition].self, from: tools)
            guard list.count == 1, let t = list.first, let fn = t.function,
                  t.type == "function",
                  fn.name == "memory",
                  fn.description == "Replace your memory document.\n\nWrite the full document each time; it is not appended."
            else {
                throw NSError(domain: "decode-check", code: 6, userInfo: [
                    NSLocalizedDescriptionKey: "tool decoded wrong: \(list)",
                ])
            }
            guard fn.parametersJSON.contains("\"required\""),
                  fn.parametersJSON.contains("\n"),        // pretty-printed, not one line
                  fn.parametersJSON.contains("\"maxLength\""),
                  !fn.parametersJSON.contains("3.0")        // integral survives as 3
            else {
                throw NSError(domain: "decode-check", code: 7, userInfo: [
                    NSLocalizedDescriptionKey: "schema not re-indented as expected: \(fn.parametersJSON)",
                ])
            }
        }
        await check("empty tool list decodes") {
            _ = try decoder.decode([ToolDefinition].self, from: Data("[]".utf8))
        }

        // The tools list is heterogeneous: a function tool plus a server tool
        // like web search — a bare {type:...} with NO function object. The
        // missing `function` key must not fail the whole array: the server tool
        // decodes as a functionless entry that still lists, showing its type,
        // and an unknown future {type:...} degrades the same way.
        let heterogeneous = Data("""
        [{"type":"function","function":{"name":"memory","description":"d","parameters":{"type":"object"}}},{"type":"openrouter:web_search"},{"type":"acme:some_future_tool"}]
        """.utf8)
        await check("heterogeneous /v1/tools: function tool + functionless server tools") {
            let list = try decoder.decode([ToolDefinition].self, from: heterogeneous)
            guard list.count == 3,
                  list[0].function?.name == "memory",
                  list[1].function == nil, list[1].type == "openrouter:web_search",
                  list[1].displayTitle == "Web search",              // humanized label
                  list[2].function == nil, list[2].displayTitle == "Some future tool"
            else {
                throw NSError(domain: "decode-check", code: 11, userInfo: [
                    NSLocalizedDescriptionKey: "heterogeneous tools decoded wrong: \(list)",
                ])
            }
        }
        // Calendar events. location and notes are omitempty on the wire, so the
        // same build has to decode a bot's fully-populated event and a bare
        // user-created one with both keys absent.
        let fullEvent = Data("""
        {"id":"evt_01M1A0000000000000000001","title":"Lunch with Alex","startsAt":"2026-08-31T12:00:00Z","endsAt":"2026-08-31T13:00:00Z","location":"Blue Bottle","notes":"bring the lease","createdBy":"bot_01M16K3W6TZ0EHQFPKZ490BDX2","createdAt":"2026-08-30T09:12:44.31Z","updatedAt":"2026-08-30T09:12:44.31Z"}
        """.utf8)
        let bareEvent = Data("""
        {"id":"evt_01M1A0000000000000000002","title":"Standup","startsAt":"2026-08-31T09:00:00Z","endsAt":"2026-08-31T09:15:00Z","createdBy":"user","createdAt":"2026-08-30T09:12:44Z","updatedAt":"2026-08-30T09:12:44Z"}
        """.utf8)
        await check("Event decodes with location/notes present and absent") {
            let full = try decoder.decode(Event.self, from: fullEvent)
            let bare = try decoder.decode(Event.self, from: bareEvent)
            guard full.location == "Blue Bottle", full.hasNotes, !full.isUserCreated,
                  bare.location == nil, bare.notes == nil, !bare.hasLocation, bare.isUserCreated
            else {
                throw NSError(domain: "decode-check", code: 12, userInfo: [
                    NSLocalizedDescriptionKey: "event optionals decoded wrong: \(full) / \(bare)",
                ])
            }
        }
        await check("empty event list decodes") {
            _ = try decoder.decode([Event].self, from: Data("[]".utf8))
        }

        // Multiple calendars landed after the first server release: an event
        // from a current botnetd always carries calendarId, one from an older
        // build never does, and the same build must decode both — nil meaning
        // "old server", never a real value.
        let eventWithCalendar = Data("""
        {"id":"evt_01M1A0000000000000000003","title":"Fed minutes","startsAt":"2026-09-02T18:00:00Z","endsAt":"2026-09-02T19:00:00Z","calendarId":"cal_01M1B0000000000000000001","createdBy":"user","createdAt":"2026-08-31T09:00:00Z","updatedAt":"2026-08-31T09:00:00Z"}
        """.utf8)
        await check("Event.calendarId decodes when present, nil when absent") {
            let filed = try decoder.decode(Event.self, from: eventWithCalendar)
            guard filed.calendarId == "cal_01M1B0000000000000000001" else {
                throw NSError(domain: "decode-check", code: 14, userInfo: [
                    NSLocalizedDescriptionKey: "calendarId decoded as \(String(describing: filed.calendarId))",
                ])
            }
            let old = try decoder.decode(Event.self, from: bareEvent)
            guard old.calendarId == nil else {
                throw NSError(domain: "decode-check", code: 15, userInfo: [
                    NSLocalizedDescriptionKey: "absent calendarId decoded as \(String(describing: old.calendarId))",
                ])
            }
        }

        // A calendar with a color this build knows, and one with a color it
        // doesn't (a newer server's enum can grow): both must decode — the
        // graceful fallback is Palette.calendar(_:)'s job, not the decoder's.
        let knownColorCalendar = Data("""
        {"id":"cal_01M1B0000000000000000001","name":"Company Earnings","color":"teal","createdBy":"user","createdAt":"2026-08-31T08:59:00.412Z","updatedAt":"2026-08-31T08:59:00.412Z"}
        """.utf8)
        let unknownColorCalendar = Data("""
        {"id":"cal_01M1B0000000000000000002","name":"Financial Updates","color":"chartreuse","createdBy":"bot_01M16K3W6TZ0EHQFPKZ490BDX2","createdAt":"2026-08-31T09:01:00Z","updatedAt":"2026-08-31T09:01:00Z"}
        """.utf8)
        await check("EventCalendar decodes with a known and an unknown color") {
            let known = try decoder.decode(EventCalendar.self, from: knownColorCalendar)
            let unknown = try decoder.decode(EventCalendar.self, from: unknownColorCalendar)
            guard known.color == "teal", known.name == "Company Earnings",
                  unknown.color == "chartreuse", unknown.createdBy.hasPrefix("bot_")
            else {
                throw NSError(domain: "decode-check", code: 16, userInfo: [
                    NSLocalizedDescriptionKey: "calendar decoded wrong: \(known) / \(unknown)",
                ])
            }
        }
        await check("empty calendar list decodes") {
            _ = try decoder.decode([EventCalendar].self, from: Data("[]".utf8))
        }

        // Recurrence landed with calendar-driven firing: an event carrying an
        // RRULE carries its IANA tz and (on an executable calendar) the
        // automation it fires. All three are omitempty on the wire, so the same
        // build must decode a firing event and every older fixture above with
        // all three absent — nil meaning "not recurring / fires nothing", the
        // Models header rule.
        let firingEvent = Data("""
        {"id":"evt_01M1A0000000000000000004","title":"fred-m2","startsAt":"2026-09-22T17:05:00Z","endsAt":"2026-09-22T17:35:00Z","calendarId":"cal_01M1B0000000000000000003","rrule":"FREQ=MONTHLY;BYDAY=4TU","tz":"America/New_York","automation":"fred-m2","createdBy":"user","createdAt":"2026-09-01T00:00:00Z","updatedAt":"2026-09-01T00:00:00Z"}
        """.utf8)
        await check("Event rrule/tz/automation decode when present, nil when absent") {
            let firing = try decoder.decode(Event.self, from: firingEvent)
            guard firing.rrule == "FREQ=MONTHLY;BYDAY=4TU",
                  firing.tz == "America/New_York",
                  firing.automation == "fred-m2",
                  firing.isRecurring, firing.firesAutomation
            else {
                throw NSError(domain: "decode-check", code: 17, userInfo: [
                    NSLocalizedDescriptionKey: "recurrence fields decoded wrong: \(firing)",
                ])
            }
            let plain = try decoder.decode(Event.self, from: fullEvent)
            guard plain.rrule == nil, plain.tz == nil, plain.automation == nil,
                  !plain.isRecurring, !plain.firesAutomation
            else {
                throw NSError(domain: "decode-check", code: 18, userInfo: [
                    NSLocalizedDescriptionKey: "absent recurrence fields decoded as real values: \(plain)",
                ])
            }
        }

        // Executable calendars: the contract sends `executable` on every
        // calendar from a current server; an older server omits it, and absent
        // must read as not-executable, never as unknown-but-maybe.
        let executableCalendar = Data("""
        {"id":"cal_01M1B0000000000000000003","name":"Automations","color":"orange","executable":true,"createdBy":"user","createdAt":"2026-09-01T00:00:00Z","updatedAt":"2026-09-01T00:00:00Z"}
        """.utf8)
        await check("EventCalendar.executable decodes when present, absent = not executable") {
            let exec = try decoder.decode(EventCalendar.self, from: executableCalendar)
            guard exec.executable == true, exec.isExecutable else {
                throw NSError(domain: "decode-check", code: 19, userInfo: [
                    NSLocalizedDescriptionKey: "executable decoded wrong: \(exec)",
                ])
            }
            let old = try decoder.decode(EventCalendar.self, from: knownColorCalendar)
            guard old.executable == nil, !old.isExecutable else {
                throw NSError(domain: "decode-check", code: 20, userInfo: [
                    NSLocalizedDescriptionKey: "absent executable decoded as \(String(describing: old.executable))",
                ])
            }
        }

        // GET /v1/instances: the calendar pane's data source. One recurring
        // event arrives as several instances sharing its eventId on different
        // days — each must keep a distinct Identifiable id or SwiftUI collapses
        // them — and a single event passes through with recurring=false.
        let instances = Data("""
        [{"eventId":"evt_01M1A0000000000000000004","calendarId":"cal_01M1B0000000000000000003","title":"fred-m2","startsAt":"2026-09-22T17:05:00Z","endsAt":"2026-09-22T17:35:00Z","automation":"fred-m2","recurring":true,"createdBy":"user"},{"eventId":"evt_01M1A0000000000000000004","calendarId":"cal_01M1B0000000000000000003","title":"fred-m2","startsAt":"2026-10-27T17:05:00Z","endsAt":"2026-10-27T17:35:00Z","automation":"fred-m2","recurring":true,"createdBy":"user"},{"eventId":"evt_01M1A0000000000000000001","calendarId":"cal_01M1B0000000000000000001","title":"Lunch with Alex","startsAt":"2026-09-02T12:00:00Z","endsAt":"2026-09-02T13:00:00Z","location":"Blue Bottle","notes":"bring the lease","recurring":false,"createdBy":"bot_01M16K3W6TZ0EHQFPKZ490BDX2"}]
        """.utf8)
        await check("instances: same event on two days, distinct ids, single pass-through") {
            let list = try decoder.decode([EventInstance].self, from: instances)
            guard list.count == 3,
                  list[0].eventId == list[1].eventId,
                  list[0].id != list[1].id,
                  list[0].recurring, list[1].recurring,
                  list[0].firesAutomation, list[0].automation == "fred-m2",
                  !list[2].recurring, list[2].location == "Blue Bottle",
                  list[2].automation == nil, !list[2].firesAutomation,
                  !list[2].isUserCreated, list[0].isUserCreated
            else {
                throw NSError(domain: "decode-check", code: 21, userInfo: [
                    NSLocalizedDescriptionKey: "instances decoded wrong: \(list)",
                ])
            }
        }
        await check("empty instances list decodes") {
            _ = try decoder.decode([EventInstance].self, from: Data("[]".utf8))
        }

        // The old-server fallback: a botnetd without /v1/instances still shows
        // its events, synthesized one-to-one. Every rendered field must survive
        // the mapping, and a plain event reads as non-recurring.
        await check("EventInstance.single preserves the event") {
            let event = try decoder.decode(Event.self, from: fullEvent)
            let single = EventInstance.single(event)
            guard single.eventId == event.id, single.title == event.title,
                  single.startsAt == event.startsAt, single.endsAt == event.endsAt,
                  single.location == event.location, single.notes == event.notes,
                  single.calendarId == event.calendarId,
                  single.createdBy == event.createdBy,
                  !single.recurring, single.automation == nil
            else {
                throw NSError(domain: "decode-check", code: 22, userInfo: [
                    NSLocalizedDescriptionKey: "single-event synthesis dropped a field: \(single)",
                ])
            }
        }

        // Automations (bridged through botnet from the stdd automations
        // service). The list row: schedule present with its raw duration
        // strings, scheduleError null, a lastRun summary, and the absolute
        // `path` the bridge adds for Open in Cursor.
        let automationRow = Data("""
        {"name":"fred-fixture","goal":"Monthly M2 fetch","dir":"fred-fixture","path":"/tmp/auto-repo/fred-fixture","schedule":{"rrule":"FREQ=MONTHLY;BYDAY=4TU","at":"13:05","tz":"America/New_York","retryEvery":"2h","retryFor":"30h"},"scheduleError":null,"freshness":"ok","lastRun":{"id":"run_01M9RUNAAAAAAAAAAAAAAAAAA1","automation":"fred-fixture","trigger":"schedule","started":"2026-08-25T17:05:02Z","finished":"2026-08-25T17:05:41Z","exitCode":0,"status":"ok","formUsed":3,"escalationReason":null}}
        """.utf8)
        await check("Automation decodes: schedule present, lastRun summary, path") {
            let a = try decoder.decode(Automation.self, from: automationRow)
            guard a.name == "fred-fixture", a.goal == "Monthly M2 fetch",
                  a.path == "/tmp/auto-repo/fred-fixture",
                  a.schedule?.rrule == "FREQ=MONTHLY;BYDAY=4TU",
                  a.schedule?.at == "13:05", a.schedule?.tz == "America/New_York",
                  a.schedule?.retryEvery == "2h", a.schedule?.retryFor == "30h",
                  a.scheduleError == nil, a.freshness == "ok",
                  a.lastRun?.trigger == "schedule", a.lastRun?.exitCode == 0,
                  a.lastRun?.formUsed == 3, a.lastRun?.escalationReason == nil,
                  a.runs == nil
            else {
                throw NSError(domain: "decode-check", code: 23, userInfo: [
                    NSLocalizedDescriptionKey: "automation row decoded wrong: \(a)",
                ])
            }
        }

        // Unscheduled: schedule null, lastRun null, and no path key (a service
        // that predates the bridge's path field) — nil, never a real value.
        let unscheduledRow = Data("""
        {"name":"plain-fixture","goal":"","dir":"nested/plain-fixture","schedule":null,"scheduleError":null,"freshness":"unscheduled","lastRun":null}
        """.utf8)
        await check("Automation decodes: schedule/lastRun null, path absent") {
            let a = try decoder.decode(Automation.self, from: unscheduledRow)
            guard a.schedule == nil, a.lastRun == nil, a.path == nil,
                  a.freshness == "unscheduled", a.scheduleError == nil
            else {
                throw NSError(domain: "decode-check", code: 24, userInfo: [
                    NSLocalizedDescriptionKey: "unscheduled row decoded wrong: \(a)",
                ])
            }
        }

        // A manifest whose schedule block failed to parse: scheduleError set,
        // treated as unscheduled server-side.
        let brokenSchedRow = Data("""
        {"name":"broken-sched","goal":"g","dir":"broken-sched","schedule":null,"scheduleError":"schedule: at \\"25:99\\" must be wall-clock HH:MM (e.g. \\"13:05\\")","freshness":"unscheduled","lastRun":null}
        """.utf8)
        await check("Automation.scheduleError decodes when set") {
            let a = try decoder.decode(Automation.self, from: brokenSchedRow)
            guard a.scheduleError?.contains("25:99") == true else {
                throw NSError(domain: "decode-check", code: 25, userInfo: [
                    NSLocalizedDescriptionKey: "scheduleError decoded wrong: \(a)",
                ])
            }
        }

        // Every freshness value the service emits must decode and map to a
        // color; an unknown future value must fall back quietly, not crash.
        await check("every freshness value decodes") {
            for f in ["ok", "pending", "stale", "failed", "never", "unscheduled", "future-value"] {
                let row = Data("""
                {"name":"x","goal":"","dir":"x","schedule":null,"scheduleError":null,"freshness":"\(f)","lastRun":null}
                """.utf8)
                let a = try decoder.decode(Automation.self, from: row)
                guard a.freshness == f else {
                    throw NSError(domain: "decode-check", code: 26, userInfo: [
                        NSLocalizedDescriptionKey: "freshness \(f) decoded as \(a.freshness)",
                    ])
                }
            }
        }

        // The detail row is the list row plus runs (newest first). A running
        // run has finished:"" — that is a string on the wire, never a Date, so
        // an in-flight run must decode and read as unfinished.
        let automationDetail = Data("""
        {"name":"fred-fixture","goal":"Monthly M2 fetch","dir":"fred-fixture","path":"/tmp/auto-repo/fred-fixture","schedule":{"rrule":"FREQ=MONTHLY;BYDAY=4TU","at":"13:05","tz":"America/New_York","retryEvery":"2h","retryFor":"30h"},"scheduleError":null,"freshness":"pending","lastRun":{"id":"run_01M9RUNAAAAAAAAAAAAAAAAAA2","automation":"fred-fixture","trigger":"manual","started":"2026-09-01T09:00:00Z","finished":"","exitCode":-1,"status":"running","formUsed":0,"escalationReason":null},"runs":[{"id":"run_01M9RUNAAAAAAAAAAAAAAAAAA2","automation":"fred-fixture","trigger":"manual","started":"2026-09-01T09:00:00Z","finished":"","exitCode":-1,"status":"running","formUsed":0,"escalationReason":null},{"id":"run_01M9RUNAAAAAAAAAAAAAAAAAA1","automation":"fred-fixture","trigger":"schedule","started":"2026-08-25T17:05:02Z","finished":"2026-08-25T17:05:41Z","exitCode":1,"status":"failed","formUsed":3,"escalationReason":"source paywalled"}]}
        """.utf8)
        await check("Automation detail: runs list, running vs finished, escalationReason") {
            let a = try decoder.decode(Automation.self, from: automationDetail)
            guard let runs = a.runs, runs.count == 2,
                  runs[0].status == "running", !runs[0].isFinished,
                  runs[0].finishedAt == nil, runs[0].startedAt != nil,
                  runs[1].isFinished, runs[1].escalationReason == "source paywalled",
                  runs[1].exitCode == 1,
                  let dur = runs[1].duration, Int(dur) == 39
            else {
                throw NSError(domain: "decode-check", code: 27, userInfo: [
                    NSLocalizedDescriptionKey: "detail runs decoded wrong: \(String(describing: a.runs))",
                ])
            }
        }

        // A FRESH automation's detail row: zero runs means the wire omits the
        // `runs` key entirely (Go's omitempty), same as lastRun null. The
        // decode must not throw, and absent must stay nil — the Store's
        // detail load is what normalizes it to [] (absence provably means
        // "no runs yet" only there; on a list row it means "not a detail").
        let freshDetail = Data("""
        {"name":"fresh-fixture","goal":"g","dir":"fresh-fixture","path":"/tmp/auto-repo/fresh-fixture","schedule":{"rrule":"FREQ=DAILY","at":"07:00","tz":"America/New_York","retryEvery":"1h","retryFor":"6h"},"scheduleError":null,"freshness":"never","lastRun":null}
        """.utf8)
        await check("fresh detail: absent runs key decodes (nil), lastRun null") {
            let a = try decoder.decode(Automation.self, from: freshDetail)
            guard a.runs == nil, a.lastRun == nil, a.freshness == "never" else {
                throw NSError(domain: "decode-check", code: 33, userInfo: [
                    NSLocalizedDescriptionKey: "fresh detail decoded wrong: \(a)",
                ])
            }
        }

        // GET /v1/runs/{id}: the summary flattened with envelope, stderrTail,
        // error. Envelope present decodes into artifacts the table renders.
        let fullRunOK = Data("""
        {"id":"run_01M9RUNAAAAAAAAAAAAAAAAAA1","automation":"fred-fixture","trigger":"schedule","started":"2026-08-25T17:05:02Z","finished":"2026-08-25T17:05:41Z","exitCode":0,"status":"ok","formUsed":3,"escalationReason":null,"envelope":{"automation":"fred-fixture","status":"ok","form_used":3,"artifacts":[{"path":"data/m2.csv","rows":312,"newest":"2026-08-29"}],"escalation_reason":null},"stderrTail":"","error":""}
        """.utf8)
        await check("RunDetail decodes: envelope artifacts, empty stderr/error") {
            let r = try decoder.decode(RunDetail.self, from: fullRunOK)
            guard r.summary.id == "run_01M9RUNAAAAAAAAAAAAAAAAAA1",
                  r.summary.status == "ok",
                  let env = r.envelope, env.artifacts.count == 1,
                  env.artifacts[0].path == "data/m2.csv",
                  env.artifacts[0].rows == 312,
                  env.artifacts[0].newest == "2026-08-29",
                  env.status == "ok", env.formUsed == 3,
                  r.stderrTail.isEmpty, r.error.isEmpty
            else {
                throw NSError(domain: "decode-check", code: 28, userInfo: [
                    NSLocalizedDescriptionKey: "full run decoded wrong: \(r)",
                ])
            }
        }

        // A no-envelope run (timeout, bad driver): envelope null, service-side
        // error and stderr tail carry the diagnosis.
        let fullRunError = Data("""
        {"id":"run_01M9RUNAAAAAAAAAAAAAAAAAA3","automation":"broken-sched","trigger":"manual","started":"2026-09-01T08:00:00Z","finished":"2026-09-01T08:00:01Z","exitCode":1,"status":"error","formUsed":0,"escalationReason":null,"envelope":null,"stderrTail":"boom: no such source\\n","error":"no envelope: stdout is empty (the driver must emit the result envelope as its last stdout line)"}
        """.utf8)
        await check("RunDetail decodes: envelope null, error + stderrTail set") {
            let r = try decoder.decode(RunDetail.self, from: fullRunError)
            guard r.envelope == nil, r.error.contains("no envelope"),
                  r.stderrTail.contains("boom") else {
                throw NSError(domain: "decode-check", code: 29, userInfo: [
                    NSLocalizedDescriptionKey: "error run decoded wrong: \(r)",
                ])
            }
        }

        // An envelope of an unknown future shape must degrade to nil, never
        // fail the whole RunDetail decode — the contract says decode leniently.
        let fullRunWeird = Data("""
        {"id":"run_01M9RUNAAAAAAAAAAAAAAAAAA4","automation":"x","trigger":"manual","started":"2026-09-01T08:00:00Z","finished":"2026-09-01T08:00:01Z","exitCode":0,"status":"ok","formUsed":3,"escalationReason":null,"envelope":{"artifacts":"not-an-array","v2":true},"stderrTail":"","error":""}
        """.utf8)
        await check("RunDetail tolerates an unknown envelope shape") {
            let r = try decoder.decode(RunDetail.self, from: fullRunWeird)
            guard r.envelope == nil, r.summary.status == "ok" else {
                throw NSError(domain: "decode-check", code: 30, userInfo: [
                    NSLocalizedDescriptionKey: "weird envelope should decode as nil: \(String(describing: r.envelope))",
                ])
            }
        }

        // POST /v1/automations/{name}/run answers 202 {runId}; a 409 while one
        // is in flight must be tellable apart from a real failure.
        await check("run-now 202 body decodes; 409 classifies as busy") {
            let accepted = try decoder.decode([String: String].self, from: Data("""
            {"runId":"run_01M9RUNAAAAAAAAAAAAAAAAAA5"}
            """.utf8))
            guard accepted["runId"]?.hasPrefix("run_") == true else {
                throw NSError(domain: "decode-check", code: 31, userInfo: [
                    NSLocalizedDescriptionKey: "202 body decoded wrong: \(accepted)",
                ])
            }
            let busy = APIClient.ServerError(message: "busy", status: 409, body: Data())
            guard APIClient.isBusy(busy), !APIClient.isBusy(
                APIClient.ServerError(message: "boom", status: 500, body: Data()))
            else {
                throw NSError(domain: "decode-check", code: 32, userInfo: [
                    NSLocalizedDescriptionKey: "isBusy misclassifies statuses",
                ])
            }
        }

        await check("empty automations list decodes") {
            _ = try decoder.decode([Automation].self, from: Data("[]".utf8))
        }

        // Projects. A project's health, nextDue and factCount are DERIVED
        // server-side and always sent; nextDue is omitted when nothing is due.
        // health stays a wire string so a value this build doesn't know renders
        // rather than throwing — Palette.health(_:) absorbs it.
        let fullProject = Data("""
        {"id":"prj_01M1C0000000000000000001","name":"China visa","goal":"Keep the visa valid through the Shanghai filing","createdBy":"user","createdAt":"2026-09-01T08:00:00Z","updatedAt":"2026-09-01T08:00:00Z","health":"overdue","nextDue":"2026-08-20T00:00:00Z","factCount":4}
        """.utf8)
        // Zero facts: no nextDue key at all, empty goal, health "unknown".
        let bareProject = Data("""
        {"id":"prj_01M1C0000000000000000002","name":"Passport","goal":"","createdBy":"bot_01M16K3W6TZ0EHQFPKZ490BDX2","createdAt":"2026-09-01T08:01:00Z","updatedAt":"2026-09-01T08:01:00Z","health":"unknown","factCount":0}
        """.utf8)
        await check("Project decodes with nextDue present and absent") {
            let full = try decoder.decode(Project.self, from: fullProject)
            let bare = try decoder.decode(Project.self, from: bareProject)
            guard full.name == "China visa", full.health == "overdue",
                  full.factCount == 4, full.isUserCreated,
                  // Overdue: the nearest due instant is already behind the
                  // project's own creation, which is what the row must say.
                  let due = full.nextDue, due < full.createdAt,
                  bare.nextDue == nil, bare.goal.isEmpty, bare.factCount == 0,
                  bare.health == "unknown", bare.createdBy.hasPrefix("bot_")
            else {
                throw NSError(domain: "decode-check", code: 40, userInfo: [
                    NSLocalizedDescriptionKey: "project decoded wrong: \(full) / \(bare)",
                ])
            }
        }

        // A health this build has never heard of must decode and reach the
        // palette's fallback, never throw the whole list's decode.
        let futureHealthProject = Data("""
        {"id":"prj_01M1C0000000000000000003","name":"SG company","goal":"g","createdBy":"user","createdAt":"2026-09-01T08:02:00Z","updatedAt":"2026-09-01T08:02:00Z","health":"escalating","factCount":2}
        """.utf8)
        await check("Project with an unknown health value decodes") {
            let p = try decoder.decode(Project.self, from: futureHealthProject)
            guard p.health == "escalating", p.healthLabel == "escalating" else {
                throw NSError(domain: "decode-check", code: 41, userInfo: [
                    NSLocalizedDescriptionKey: "unknown health decoded wrong: \(p)",
                ])
            }
        }

        // Go marshals a *time.Time as null and a zero time.Time as year 1;
        // either way the app must read "nothing due", never a year-1 date on
        // the sidebar row.
        let zeroNextDueProject = Data("""
        {"id":"prj_01M1C0000000000000000004","name":"Zero","goal":"","createdBy":"user","createdAt":"2026-09-01T08:03:00Z","updatedAt":"2026-09-01T08:03:00Z","health":"ok","nextDue":"0001-01-01T00:00:00Z","factCount":1}
        """.utf8)
        await check("Project nextDue zero-time sentinel reads as nil") {
            let p = try decoder.decode(Project.self, from: zeroNextDueProject)
            guard p.nextDue == nil else {
                throw NSError(domain: "decode-check", code: 42, userInfo: [
                    NSLocalizedDescriptionKey: "zero nextDue decoded as \(String(describing: p.nextDue))",
                ])
            }
        }
        await check("empty project list decodes") {
            _ = try decoder.decode([Project].self, from: Data("[]".utf8))
        }

        // Facts. Each kind carries a different subset of the optional keys, and
        // the same build has to decode all four: due only on deadline/recurring,
        // rrule+tz only on recurring, blocker only on milestone, body anywhere.
        let deadlineFact = Data("""
        {"id":"fct_01M1D0000000000000000001","projectId":"prj_01M1C0000000000000000001","kind":"deadline","title":"Visa expires","due":"2026-08-20T00:00:00Z","leadDays":45,"done":false,"body":"Renew at the Shanghai consulate","eventId":"evt_01M1A0000000000000000009","createdBy":"user","createdAt":"2026-09-01T08:00:00Z","updatedAt":"2026-09-01T08:00:00Z"}
        """.utf8)
        let recurringFact = Data("""
        {"id":"fct_01M1D0000000000000000002","projectId":"prj_01M1C0000000000000000003","kind":"recurring","title":"Annual return","due":"2026-11-30T00:00:00Z","leadDays":30,"rrule":"FREQ=YEARLY;BYMONTH=11;BYMONTHDAY=30","tz":"Asia/Singapore","done":false,"createdBy":"bot_01M16K3W6TZ0EHQFPKZ490BDX2","createdAt":"2026-09-01T08:02:00Z","updatedAt":"2026-09-01T08:02:00Z"}
        """.utf8)
        // Milestone: no due key at all, a blocker naming the human gate.
        let milestoneFact = Data("""
        {"id":"fct_01M1D0000000000000000003","projectId":"prj_01M1C0000000000000000001","kind":"milestone","title":"Lease signed","leadDays":0,"done":false,"blocker":"Landlord has not countersigned","createdBy":"user","createdAt":"2026-09-01T08:04:00Z","updatedAt":"2026-09-01T08:04:00Z"}
        """.utf8)
        // Note: no due, no blocker, body only — the minimum legal fact.
        let noteFact = Data("""
        {"id":"fct_01M1D0000000000000000004","projectId":"prj_01M1C0000000000000000001","kind":"note","title":"Consulate hours","leadDays":0,"done":false,"body":"Mon-Thu 09:00-11:30 only.","createdBy":"user","createdAt":"2026-09-01T08:05:00Z","updatedAt":"2026-09-01T08:05:00Z"}
        """.utf8)
        await check("ProjectFact: deadline/recurring carry due, milestone/note omit it") {
            let deadline = try decoder.decode(ProjectFact.self, from: deadlineFact)
            let recurring = try decoder.decode(ProjectFact.self, from: recurringFact)
            let milestone = try decoder.decode(ProjectFact.self, from: milestoneFact)
            let note = try decoder.decode(ProjectFact.self, from: noteFact)
            guard deadline.kind == "deadline", deadline.due != nil,
                  deadline.leadDays == 45, !deadline.done,
                  deadline.body == "Renew at the Shanghai consulate",
                  deadline.eventId == "evt_01M1A0000000000000000009",
                  deadline.rrule == nil, deadline.tz == nil, deadline.blocker == nil,
                  !deadline.isBlocked,
                  recurring.due != nil, recurring.rrule?.hasPrefix("FREQ=YEARLY") == true,
                  recurring.tz == "Asia/Singapore", recurring.leadDays == 30,
                  recurring.eventId == nil, recurring.body == nil,
                  milestone.due == nil, milestone.blocker == "Landlord has not countersigned",
                  milestone.isBlocked, milestone.rrule == nil, milestone.tz == nil,
                  note.due == nil, note.blocker == nil, !note.isBlocked,
                  note.body == "Mon-Thu 09:00-11:30 only.", note.eventId == nil
            else {
                throw NSError(domain: "decode-check", code: 43, userInfo: [
                    NSLocalizedDescriptionKey: "fact optionals decoded wrong: \(deadline) / \(recurring) / \(milestone) / \(note)",
                ])
            }
        }

        // Same zero-time rule as nextDue: a server that marshals an unset due as
        // year 1 rather than omitting the key must still read as "no due date".
        let zeroDueFact = Data("""
        {"id":"fct_01M1D0000000000000000005","projectId":"prj_01M1C0000000000000000001","kind":"note","title":"Zero due","due":"0001-01-01T00:00:00Z","leadDays":0,"done":false,"createdBy":"user","createdAt":"2026-09-01T08:06:00Z","updatedAt":"2026-09-01T08:06:00Z"}
        """.utf8)
        await check("ProjectFact due zero-time sentinel reads as nil") {
            let f = try decoder.decode(ProjectFact.self, from: zeroDueFact)
            guard f.due == nil else {
                throw NSError(domain: "decode-check", code: 44, userInfo: [
                    NSLocalizedDescriptionKey: "zero due decoded as \(String(describing: f.due))",
                ])
            }
        }

        // A kind this build doesn't know must decode and render with the
        // fallback glyph, exactly like an unknown health.
        let futureKindFact = Data("""
        {"id":"fct_01M1D0000000000000000006","projectId":"prj_01M1C0000000000000000001","kind":"audit","title":"Books reviewed","leadDays":0,"done":true,"createdBy":"user","createdAt":"2026-09-01T08:07:00Z","updatedAt":"2026-09-01T08:07:00Z"}
        """.utf8)
        await check("ProjectFact with an unknown kind decodes") {
            let f = try decoder.decode(ProjectFact.self, from: futureKindFact)
            guard f.kind == "audit", f.done, FactKind(rawValue: f.kind) == nil,
                  !f.isCompletable
            else {
                throw NSError(domain: "decode-check", code: 45, userInfo: [
                    NSLocalizedDescriptionKey: "unknown kind decoded wrong: \(f)",
                ])
            }
        }

        // GET /v1/projects/{id}: the project plus its facts, urgency-first as
        // the server sorted them. A project with no facts sends `facts` as null
        // (Go's nil slice), which must read as an empty list, not a decode
        // failure — the pane says "no facts yet" off exactly that.
        let projectDetail = Data("""
        {"project":{"id":"prj_01M1C0000000000000000001","name":"China visa","goal":"Keep the visa valid","createdBy":"user","createdAt":"2026-09-01T08:00:00Z","updatedAt":"2026-09-01T08:00:00Z","health":"overdue","nextDue":"2026-08-20T00:00:00Z","factCount":2},"facts":[{"id":"fct_01M1D0000000000000000001","projectId":"prj_01M1C0000000000000000001","kind":"deadline","title":"Visa expires","due":"2026-08-20T00:00:00Z","leadDays":45,"done":false,"createdBy":"user","createdAt":"2026-09-01T08:00:00Z","updatedAt":"2026-09-01T08:00:00Z"},{"id":"fct_01M1D0000000000000000003","projectId":"prj_01M1C0000000000000000001","kind":"milestone","title":"Lease signed","leadDays":0,"done":false,"blocker":"Landlord has not countersigned","createdBy":"user","createdAt":"2026-09-01T08:04:00Z","updatedAt":"2026-09-01T08:04:00Z"}]}
        """.utf8)
        let emptyDetail = Data("""
        {"project":{"id":"prj_01M1C0000000000000000002","name":"Passport","goal":"","createdBy":"user","createdAt":"2026-09-01T08:01:00Z","updatedAt":"2026-09-01T08:01:00Z","health":"unknown","factCount":0},"facts":null}
        """.utf8)
        await check("ProjectDetail decodes; a null facts list reads as empty") {
            let detail = try decoder.decode(ProjectDetail.self, from: projectDetail)
            let empty = try decoder.decode(ProjectDetail.self, from: emptyDetail)
            guard detail.project.name == "China visa", detail.facts.count == 2,
                  detail.facts[0].kind == "deadline", detail.facts[1].isBlocked,
                  empty.facts.isEmpty, empty.project.factCount == 0
            else {
                throw NSError(domain: "decode-check", code: 46, userInfo: [
                    NSLocalizedDescriptionKey: "project detail decoded wrong: \(detail) / \(empty)",
                ])
            }
        }

        // Hierarchy and severity (rock 1). parentId is omitempty, so a
        // top-level project sends NO key at all; a server that sends "" must
        // read as the same thing, because the sidebar has exactly one notion
        // of "root" and two spellings of it would split the tree in half.
        let childProject = Data("""
        {"id":"prj_01M1C0000000000000000011","name":"Passport","goal":"","parentId":"prj_01M1C0000000000000000010","createdBy":"user","createdAt":"2026-09-01T08:10:00Z","updatedAt":"2026-09-01T08:10:00Z","health":"overdue","severity":"S0","nextDue":"2026-08-20T00:00:00Z","factCount":2,"childCount":0}
        """.utf8)
        let rootProject = Data("""
        {"id":"prj_01M1C0000000000000000010","name":"Document Expirations","goal":"Nothing lapses","createdBy":"user","createdAt":"2026-09-01T08:09:00Z","updatedAt":"2026-09-01T08:09:00Z","health":"overdue","severity":"S0","nextDue":"2026-08-20T00:00:00Z","factCount":0,"childCount":3}
        """.utf8)
        let emptyParentProject = Data("""
        {"id":"prj_01M1C0000000000000000012","name":"Loose","goal":"","parentId":"","createdBy":"user","createdAt":"2026-09-01T08:11:00Z","updatedAt":"2026-09-01T08:11:00Z","health":"ok","severity":"S2","factCount":1,"childCount":0}
        """.utf8)
        await check("Project.parentId: present, absent and \"\" (absent and \"\" are both root)") {
            let child = try decoder.decode(Project.self, from: childProject)
            let root = try decoder.decode(Project.self, from: rootProject)
            let empty = try decoder.decode(Project.self, from: emptyParentProject)
            guard child.parentId == "prj_01M1C0000000000000000010", !child.isTopLevel,
                  root.parentId == nil, root.isTopLevel,
                  empty.parentId == nil, empty.isTopLevel
            else {
                throw NSError(domain: "decode-check", code: 48, userInfo: [
                    NSLocalizedDescriptionKey: "parentId decoded wrong: \(child) / \(root) / \(empty)",
                ])
            }
        }

        // childCount is omitempty too: a leaf sends no key, and absent IS 0.
        await check("Project.childCount: present, and absent reads as 0") {
            let root = try decoder.decode(Project.self, from: rootProject)
            let leaf = try decoder.decode(Project.self, from: bareProject)
            guard root.childCount == 3, root.hasChildren,
                  leaf.childCount == 0, !leaf.hasChildren
            else {
                throw NSError(domain: "decode-check", code: 49, userInfo: [
                    NSLocalizedDescriptionKey: "childCount decoded wrong: \(root) / \(leaf)",
                ])
            }
        }

        // Severity is DERIVED from the rolled-up health, so it is a wire string
        // for the same reason health is: an S3 from a newer server must render,
        // not throw. A server that predates severity sends no key, which reads
        // as "" — and that must not be mistaken for a real severity.
        let severities = ["S0", "S1", "S2", "S7"]
        await check("Project.severity: S0/S1/S2, an unknown value, and absent") {
            for value in severities {
                let data = Data("""
                {"id":"prj_sev_\(value)","name":"Sev \(value)","goal":"","createdBy":"user","createdAt":"2026-09-01T08:12:00Z","updatedAt":"2026-09-01T08:12:00Z","health":"ok","severity":"\(value)","factCount":0}
                """.utf8)
                let p = try decoder.decode(Project.self, from: data)
                guard p.severity == value, p.hasSeverity else {
                    throw NSError(domain: "decode-check", code: 50, userInfo: [
                        NSLocalizedDescriptionKey: "severity \(value) decoded as \(p.severity)",
                    ])
                }
            }
            // The pre-severity server: no key, and the app must know it has none
            // rather than treating "" as a severity of its own.
            let old = try decoder.decode(Project.self, from: bareProject)
            guard old.severity.isEmpty, !old.hasSeverity else {
                throw NSError(domain: "decode-check", code: 50, userInfo: [
                    NSLocalizedDescriptionKey: "absent severity decoded as \(old.severity)",
                ])
            }
        }

        // GET /v1/projects/{id} gains `children`: the DIRECT children with
        // their own derived fields. Absent (old server) and null (Go nil slice)
        // are both an empty list, exactly like `facts`.
        let detailWithChildren = Data("""
        {"project":{"id":"prj_01M1C0000000000000000010","name":"Document Expirations","goal":"Nothing lapses","createdBy":"user","createdAt":"2026-09-01T08:09:00Z","updatedAt":"2026-09-01T08:09:00Z","health":"overdue","severity":"S0","nextDue":"2026-08-20T00:00:00Z","factCount":0,"childCount":2},"facts":null,"children":[{"id":"prj_01M1C0000000000000000011","name":"Passport","goal":"","parentId":"prj_01M1C0000000000000000010","createdBy":"user","createdAt":"2026-09-01T08:10:00Z","updatedAt":"2026-09-01T08:10:00Z","health":"overdue","severity":"S0","nextDue":"2026-08-20T00:00:00Z","factCount":2},{"id":"prj_01M1C0000000000000000013","name":"China Q2 Visa","goal":"","parentId":"prj_01M1C0000000000000000010","createdBy":"user","createdAt":"2026-09-01T08:13:00Z","updatedAt":"2026-09-01T08:13:00Z","health":"due_soon","severity":"S1","nextDue":"2026-09-20T00:00:00Z","factCount":1}]}
        """.utf8)
        let detailNullChildren = Data("""
        {"project":{"id":"prj_01M1C0000000000000000011","name":"Passport","goal":"","parentId":"prj_01M1C0000000000000000010","createdBy":"user","createdAt":"2026-09-01T08:10:00Z","updatedAt":"2026-09-01T08:10:00Z","health":"overdue","severity":"S0","factCount":2},"facts":null,"children":null}
        """.utf8)
        await check("ProjectDetail.children: present, null and absent") {
            let withKids = try decoder.decode(ProjectDetail.self, from: detailWithChildren)
            let nullKids = try decoder.decode(ProjectDetail.self, from: detailNullChildren)
            // `projectDetail` above predates the key entirely.
            let absentKids = try decoder.decode(ProjectDetail.self, from: projectDetail)
            guard withKids.children.count == 2,
                  withKids.children[0].name == "Passport",
                  withKids.children[0].severity == "S0",
                  withKids.children[0].parentId == withKids.project.id,
                  withKids.children[1].severity == "S1",
                  nullKids.children.isEmpty, absentKids.children.isEmpty
            else {
                throw NSError(domain: "decode-check", code: 51, userInfo: [
                    NSLocalizedDescriptionKey: "children decoded wrong: \(withKids) / \(nullKids) / \(absentKids)",
                ])
            }
        }

        // The tree is built ONCE from the flat list, and every view reads it —
        // so the shape it produces is asserted here rather than eyeballed in a
        // screenshot. The exhaustive cases (orphans, cycles, ordering) live in
        // the scratchpad harness; this is the contract's own 3-level example.
        await check("ProjectTree builds the contract's 3-level example") {
            let flat = try decoder.decode([Project].self, from: Data("""
            [{"id":"p1","name":"Document Expirations","createdAt":"2026-09-01T08:00:00Z","updatedAt":"2026-09-01T08:00:00Z","health":"overdue","severity":"S0","childCount":2},
             {"id":"p2","name":"Passport","parentId":"p1","createdAt":"2026-09-01T08:00:00Z","updatedAt":"2026-09-01T08:00:00Z","health":"overdue","severity":"S0","childCount":1},
             {"id":"p3","name":"Renewal appointment","parentId":"p2","createdAt":"2026-09-01T08:00:00Z","updatedAt":"2026-09-01T08:00:00Z","health":"ok","severity":"S2"},
             {"id":"p4","name":"China Q2 Visa","parentId":"p1","createdAt":"2026-09-01T08:00:00Z","updatedAt":"2026-09-01T08:00:00Z","health":"due_soon","severity":"S1"},
             {"id":"p5","name":"Singapore Pte Ltd","createdAt":"2026-09-01T08:00:00Z","updatedAt":"2026-09-01T08:00:00Z","health":"ok","severity":"S2"}]
            """.utf8))
            let tree = ProjectTree(flat)
            guard tree.roots.map(\.id) == ["p1", "p5"],
                  tree.children(of: "p1").map(\.id) == ["p2", "p4"],
                  tree.depth(of: "p3") == 2, tree.depth(of: "p1") == 0,
                  tree.subtree(of: "p1").map(\.id) == ["p1", "p2", "p3", "p4"],
                  tree.rows(expanded: ["p1", "p2"]).map(\.project.id)
                      == ["p1", "p2", "p3", "p4", "p5"],
                  tree.rows(expanded: []).map(\.project.id) == ["p1", "p5"],
                  // The Edit sheet's Parent picker: everything outside this
                  // project's own subtree, so a cycle is never offered.
                  tree.parentCandidates(for: "p2").map(\.project.id) == ["p1", "p4", "p5"],
                  tree.parentCandidates(for: "p1").map(\.project.id) == ["p5"]
            else {
                throw NSError(domain: "decode-check", code: 52, userInfo: [
                    NSLocalizedDescriptionKey: "tree shape wrong: roots \(tree.roots.map(\.id)), rows \(tree.rows(expanded: ["p1", "p2"]).map(\.project.id))",
                ])
            }
        }

        // The four kinds the sheet offers are a Swift enum with a field table;
        // the table is what decides which inputs show, so it is checked here
        // rather than only by eye in a screenshot.
        await check("FactKind field table matches the contract's validation") {
            guard FactKind.deadline.fields == [.due, .leadDays, .body],
                  FactKind.recurring.fields == [.due, .leadDays, .rrule, .tz, .body],
                  FactKind.milestone.fields == [.blocker, .body],
                  FactKind.note.fields == [.body],
                  FactKind.allCases.count == 4,
                  FactKind.deadline.isCompletable, FactKind.milestone.isCompletable,
                  !FactKind.recurring.isCompletable, !FactKind.note.isCompletable
            else {
                throw NSError(domain: "decode-check", code: 47, userInfo: [
                    NSLocalizedDescriptionKey: "FactKind field table drifted from the contract",
                ])
            }
        }

        // The times the app sends back must be the RFC3339 form the server
        // parses, and must survive a round trip through its own decoder.
        await check("wireTime round-trips through the decoder") {
            let sent = Data("""
            {"id":"evt_x","title":"t","startsAt":"\(APIClient.wireTime(Date(timeIntervalSince1970: 1_790_000_000)))","endsAt":"\(APIClient.wireTime(Date(timeIntervalSince1970: 1_790_003_600)))","createdBy":"user","createdAt":"2026-08-30T09:12:44Z","updatedAt":"2026-08-30T09:12:44Z"}
            """.utf8)
            let event = try decoder.decode(Event.self, from: sent)
            guard Int(event.startsAt.timeIntervalSince1970) == 1_790_000_000,
                  event.endsAt.timeIntervalSince(event.startsAt) == 3600
            else {
                throw NSError(domain: "decode-check", code: 13, userInfo: [
                    NSLocalizedDescriptionKey: "wireTime round trip drifted: \(event)",
                ])
            }
        }

        // The absent-route path: an older botnetd 404s /v1/tools, which the app
        // must read as "hide the section", never as an error.
        await check("404 maps to isUnimplemented") {
            let err = APIClient.ServerError(message: "not found", status: 404, body: Data())
            guard APIClient.isUnimplemented(err), !APIClient.isUnimplemented(
                APIClient.ServerError(message: "boom", status: 500, body: Data()))
            else {
                throw NSError(domain: "decode-check", code: 8, userInfo: [
                    NSLocalizedDescriptionKey: "isUnimplemented misclassifies statuses",
                ])
            }
        }

        // A failed decode must say which endpoint produced it; the bare
        // DecodingError alert ("The data couldn't be read…") names nothing.
        await check("decode failure names the endpoint") {
            do {
                let _: Message = try api.decode(Data("[]".utf8), from: "/v1/decode-check-probe")
                throw NSError(domain: "decode-check", code: 2, userInfo: [
                    NSLocalizedDescriptionKey: "shape mismatch unexpectedly decoded",
                ])
            } catch let e as APIClient.ServerError {
                guard e.message.contains("/v1/decode-check-probe") else {
                    throw NSError(domain: "decode-check", code: 3, userInfo: [
                        NSLocalizedDescriptionKey: "endpoint missing from message: \(e.message)",
                    ])
                }
            }
        }

        // Live server: decode every bot's real GET responses through APIClient's
        // own transport, so one legacy row in the real DB fails here and names
        // its bot. Read-only; skipped (not failed) when no server is up.
        do {
            let bots = try await api.listBots()
            print("PASS live GET /v1/bots (\(bots.count) bots)")
            await check("live GET /v1/models") { _ = try await api.listModels() }
            await check("live GET /v1/tools (404 = older server, tolerated)") {
                do { _ = try await api.listTools() }
                catch let e where APIClient.isUnimplemented(e) {}
            }
            await check("live GET /v1/config") { _ = try await api.hasKey() }
            await check("live GET /v1/events (404 = older server, tolerated)") {
                do { _ = try await api.listEvents() }
                catch let e where APIClient.isUnimplemented(e) {}
            }
            await check("live GET /v1/events?from=&to= (404 tolerated)") {
                let now = Date()
                do { _ = try await api.listEvents(from: now, to: now.addingTimeInterval(86_400)) }
                catch let e where APIClient.isUnimplemented(e) {}
            }
            await check("live GET /v1/calendars (404 = older server, tolerated)") {
                do { _ = try await api.listCalendars() }
                catch let e where APIClient.isUnimplemented(e) {}
            }
            await check("live GET /v1/instances?from=&to= (404 = older server, tolerated)") {
                let now = Date()
                do { _ = try await api.listInstances(from: now, to: now.addingTimeInterval(86_400)) }
                catch let e where APIClient.isUnimplemented(e) {}
            }
            await check("live GET /v1/automations (404 = unmounted bridge, tolerated)") {
                do { _ = try await api.listAutomations() }
                catch let e where APIClient.isUnimplemented(e) {}
            }
            // Read-only, like every live probe here: the list, and the detail
            // of whatever it returned. A 404 is an older server, not a failure.
            await check("live GET /v1/projects (404 = older server, tolerated)") {
                do {
                    let projects = try await api.listProjects()
                    for p in projects { _ = try await api.project(p.id) }
                } catch let e where APIClient.isUnimplemented(e) {}
            }
            // The flat list has to be a well-formed tree on its own: every
            // parentId resolves inside it, and the server's childCount agrees
            // with what the client can actually draw. A server that disagreed
            // would render a project the sidebar silently hides. An older
            // server sends neither field, which is a flat tree and passes.
            await check("live /v1/projects is a consistent tree (parents resolve, childCount agrees)") {
                do {
                    let projects = try await api.listProjects()
                    let tree = ProjectTree(projects)
                    let ids = Set(projects.map(\.id))
                    for p in projects {
                        if let parent = p.parentId, !ids.contains(parent) {
                            throw NSError(domain: "decode-check", code: 53, userInfo: [
                                NSLocalizedDescriptionKey: "\(p.name) has parentId \(parent), which is not in the list",
                            ])
                        }
                        guard tree.children(of: p.id).count == p.childCount else {
                            throw NSError(domain: "decode-check", code: 53, userInfo: [
                                NSLocalizedDescriptionKey: "\(p.name): server says childCount \(p.childCount), the list has \(tree.children(of: p.id).count)",
                            ])
                        }
                        // The detail's own children must be the same set the
                        // tree drew, or the pane's strip and the sidebar
                        // disagree about what hangs under this project.
                        let detail = try await api.project(p.id)
                        guard Set(detail.children.map(\.id))
                                == Set(tree.children(of: p.id).map(\.id)) else {
                            throw NSError(domain: "decode-check", code: 53, userInfo: [
                                NSLocalizedDescriptionKey: "\(p.name): detail children \(detail.children.map(\.name)) != list children \(tree.children(of: p.id).map(\.name))",
                            ])
                        }
                    }
                    // Every project is reachable from a root: an unreachable
                    // one would exist on the wire and nowhere in the UI.
                    let reachable = Set(tree.roots.flatMap { tree.subtree(of: $0.id) }.map(\.id))
                    guard reachable == ids else {
                        throw NSError(domain: "decode-check", code: 53, userInfo: [
                            NSLocalizedDescriptionKey: "unreachable projects: \(ids.subtracting(reachable))",
                        ])
                    }
                } catch let e where APIClient.isUnimplemented(e) {}
            }
            for b in bots {
                await check("live GET /v1/bots/\(b.id)/messages") {
                    _ = try await api.messages(b.id)
                }
                await check("live GET /v1/bots/\(b.id)/segments") {
                    _ = try await api.segments(b.id)
                }
            }
        } catch let e as URLError {
            print("SKIP live GETs: no server at \(api.base) (\(e.code))")
        } catch {
            failed = true
            print("FAIL live GET /v1/bots: \(error)")
        }

        exit(failed ? 1 : 0)
    }
}
