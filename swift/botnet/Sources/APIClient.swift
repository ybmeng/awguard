// APIClient.swift — HTTP calls to the local botnetd server. This is the whole
// contract between the app and the state owner; matches go/botnet/server.go.

import Foundation

struct APIClient {
    var base = URL(string: "http://127.0.0.1:8730")!

    struct ServerError: LocalizedError {
        var message: String
        var errorDescription: String? { message }
    }

    private var decoder: JSONDecoder {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .iso8601
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

    func send(_ content: String, to botID: String) async throws -> [Message] {
        try await send("/v1/bots/\(botID)/messages", method: "POST", body: ["content": content])
    }

    // MARK: transport

    private func get<T: Decodable>(_ path: String) async throws -> T {
        try decode(await raw(path, method: "GET", body: nil))
    }

    private func send<T: Decodable>(_ path: String, method: String, body: [String: String]) async throws -> T {
        let data = try JSONSerialization.data(withJSONObject: body)
        return try decode(await raw(path, method: method, body: data))
    }

    private func decode<T: Decodable>(_ data: Data) throws -> T {
        try decoder.decode(T.self, from: data)
    }

    @discardableResult
    private func raw(_ path: String, method: String, body: Data?) async throws -> Data {
        var req = URLRequest(url: base.appendingPathComponent(path))
        req.httpMethod = method
        if let body {
            req.httpBody = body
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let (data, resp) = try await URLSession.shared.data(for: req)
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        guard (200..<300).contains(code) else {
            if let err = try? JSONDecoder().decode([String: String].self, from: data), let m = err["error"] {
                throw ServerError(message: m)
            }
            throw ServerError(message: "server error (\(code))")
        }
        return data
    }
}
