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
