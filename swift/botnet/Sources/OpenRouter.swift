// OpenRouter.swift — minimal chat-completions client. Blocking (no streaming)
// for MVP; the key lives in the Keychain.

import Foundation

struct OpenRouterClient {
    var apiKey: String

    struct APIError: LocalizedError {
        var message: String
        var errorDescription: String? { message }
    }

    private struct WireMessage: Codable {
        var role: String   // "system" | "user" | "assistant"
        var content: String
    }
    private struct Request: Codable {
        var model: String
        var messages: [WireMessage]
    }
    private struct Response: Codable {
        struct Choice: Codable {
            struct Msg: Codable { var content: String? }
            var message: Msg
        }
        struct Err: Codable { var message: String? }
        var choices: [Choice]?
        var error: Err?
    }

    /// Sends the full conversation (system prompt + every message, no
    /// compaction) and returns the assistant reply text.
    func complete(bot: Bot, history: [Message]) async throws -> String {
        guard !apiKey.isEmpty else {
            throw APIError(message: "No OpenRouter API key set — add it in Settings.")
        }
        let slug = ModelOption(name: "", id: bot.model).openRouterSlug
        var wire: [WireMessage] = []
        if !bot.systemPrompt.isEmpty {
            wire.append(WireMessage(role: "system", content: bot.systemPrompt))
        }
        for m in history {
            switch m.role {
            case .user: wire.append(WireMessage(role: "user", content: m.content))
            case .bot: wire.append(WireMessage(role: "assistant", content: m.content))
            case .system: continue // local status/error notes never go to the model
            }
        }

        var req = URLRequest(url: URL(string: "https://openrouter.ai/api/v1/chat/completions")!)
        req.httpMethod = "POST"
        req.setValue("Bearer \(apiKey)", forHTTPHeaderField: "Authorization")
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONEncoder().encode(Request(model: slug, messages: wire))

        let (data, resp) = try await URLSession.shared.data(for: req)
        let decoded = try? JSONDecoder().decode(Response.self, from: data)
        if let msg = decoded?.error?.message {
            throw APIError(message: msg)
        }
        guard (resp as? HTTPURLResponse)?.statusCode == 200,
              let content = decoded?.choices?.first?.message.content else {
            let body = String(data: data.prefix(300), encoding: .utf8) ?? ""
            throw APIError(message: "OpenRouter error (\((resp as? HTTPURLResponse)?.statusCode ?? 0)): \(body)")
        }
        return content
    }
}

enum Keychain {
    private static let service = "com.anywatch.botnet.openrouter"

    static var apiKey: String {
        get {
            let query: [String: Any] = [
                kSecClass as String: kSecClassGenericPassword,
                kSecAttrService as String: service,
                kSecReturnData as String: true,
            ]
            var item: CFTypeRef?
            guard SecItemCopyMatching(query as CFDictionary, &item) == errSecSuccess,
                  let data = item as? Data else { return "" }
            return String(data: data, encoding: .utf8) ?? ""
        }
        set {
            let base: [String: Any] = [
                kSecClass as String: kSecClassGenericPassword,
                kSecAttrService as String: service,
            ]
            SecItemDelete(base as CFDictionary)
            guard !newValue.isEmpty else { return }
            var add = base
            add[kSecValueData as String] = Data(newValue.utf8)
            SecItemAdd(add as CFDictionary, nil)
        }
    }
}
