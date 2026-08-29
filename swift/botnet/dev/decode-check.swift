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
            await check("live GET /v1/config") { _ = try await api.hasKey() }
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
