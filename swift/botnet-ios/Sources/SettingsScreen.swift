// SettingsScreen.swift — which botnetd this phone talks to, and whether it is
// answering. The only setting the app has: everything else the app knows is
// state the server owns.
//
// The address is committed explicitly, never on every keystroke — a half-typed
// host would rebuild the store once per character and probe each one.

import SwiftUI

struct SettingsScreen: View {
    @AppStorage(BaseURL.key) private var baseURL = BaseURL.fallback

    @State private var draft = ""
    @State private var connection: ConnectionState = .idle
    @FocusState private var addressFocused: Bool

    private var trimmed: String { draft.trimmingCharacters(in: .whitespacesAndNewlines) }
    private var dirty: Bool { trimmed != baseURL && !trimmed.isEmpty }

    var body: some View {
        Form {
            Section {
                TextField("Server", text: $draft)
                    .font(TypeScale.phoneMessage)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .keyboardType(.URL)
                    .submitLabel(.done)
                    .focused($addressFocused)
                    .onSubmit(commit)
                if dirty {
                    Button("Connect to \(trimmed)", action: commit)
                        .font(TypeScale.phoneRowTitle)
                }
            } header: {
                Text("botnetd address")
            } footer: {
                Text("The simulator shares the Mac's network stack, so 127.0.0.1 reaches a daemon running on this Mac. A phone on the same Wi-Fi needs the Mac's LAN address instead.")
            }

            Section {
                status
                Button("Check again") { probe(baseURL) }
                    .font(TypeScale.phoneMessage)
                    .disabled(connection == .probing)
            } header: {
                Text("Connection")
            }
        }
        .navigationTitle("Settings")
        .navigationBarTitleDisplayMode(.inline)
        .onAppear {
            draft = baseURL
            probe(baseURL)
        }
    }

    private var status: some View {
        HStack(spacing: 8) {
            Circle()
                .fill(connection.tint)
                .frame(width: Metric.healthDot, height: Metric.healthDot)
            VStack(alignment: .leading, spacing: 2) {
                Text(connection.label)
                    .font(TypeScale.phoneMessage)
                    .foregroundStyle(Palette.primaryText)
                Text(baseURL)
                    .font(TypeScale.phoneRowMeta)
                    .foregroundStyle(Palette.secondaryText)
                    .lineLimit(1)
                    .truncationMode(.middle)
            }
            Spacer(minLength: 0)
            if connection == .probing { ProgressView() }
        }
        .frame(minHeight: Metric.phoneTapTarget)
    }

    // Writing @AppStorage is what rebuilds the store (the app watches this key),
    // so the commit is the whole switch: address, store, and probe.
    private func commit() {
        guard dirty else { return }
        let url = trimmed
        baseURL = url
        addressFocused = false
        probe(url)
    }

    private func probe(_ url: String) {
        connection = .probing
        Task {
            connection = await HealthProbe.run(url)
        }
    }
}

/// What the Connection row says. A state machine rather than two booleans: the
/// row has to tell "never asked" apart from "asked and it did not answer", and
/// a failure carries the reason a user can act on (wrong port, nothing running).
enum ConnectionState: Equatable {
    case idle
    case probing
    case connected
    case unreachable(String)

    var label: String {
        switch self {
        case .idle: return "Not checked"
        case .probing: return "Checking…"
        case .connected: return "Connected"
        case .unreachable(let why): return "Unreachable — \(why)"
        }
    }

    var tint: Color {
        switch self {
        case .connected: return Palette.healthOK
        case .unreachable: return Palette.healthOverdue
        case .idle, .probing: return Palette.secondaryText
        }
    }
}

/// GET /v1/health against a bare address. Deliberately not on the shared
/// APIClient: this asks whether a server the app is NOT yet pointed at is
/// answering, which no method bound to one base URL can do, and it reads only
/// the status line so an old botnetd's body shape can't fail the check.
enum HealthProbe {
    static func run(_ base: String, timeout: TimeInterval = 4) async -> ConnectionState {
        guard let url = URL(string: base.hasSuffix("/") ? base + "v1/health" : base + "/v1/health"),
              url.host != nil
        else { return .unreachable("not a URL") }

        var request = URLRequest(url: url)
        request.timeoutInterval = timeout
        do {
            let (_, response) = try await URLSession.shared.data(for: request)
            let code = (response as? HTTPURLResponse)?.statusCode ?? 0
            guard (200..<300).contains(code) else { return .unreachable("HTTP \(code)") }
            return .connected
        } catch {
            return .unreachable((error as? URLError)?.shortReason ?? error.localizedDescription)
        }
    }
}

private extension URLError {
    /// The two failures that actually happen here — nothing listening, or a
    /// hostname that does not resolve — said in words that fit a row.
    var shortReason: String {
        switch code {
        case .cannotConnectToHost: return "nothing listening"
        case .cannotFindHost: return "unknown host"
        case .timedOut: return "timed out"
        default: return localizedDescription
        }
    }
}
