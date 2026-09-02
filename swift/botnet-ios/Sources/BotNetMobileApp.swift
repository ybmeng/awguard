// BotNetMobileApp.swift — the phone app's entry point and its one piece of
// local state: which botnetd it talks to.
//
// Everything else is shared with the Mac app by path (Models, APIClient, Store,
// Transcript, DesignSystem). The phone adds screens, never a second client.

import SwiftUI

@main
struct BotNetMobileApp: App {
    @AppStorage(BaseURL.key) private var baseURL = BaseURL.fallback
    @StateObject private var host = StoreHost(baseURL: BaseURL.stored)

    var body: some Scene {
        WindowGroup {
            RootScreen()
                .environmentObject(host.store)
                // A new server is a new world: drafts, open sheets and loaded
                // conversations all belong to the old one.
                .id(host.generation)
                .onChange(of: baseURL) { _, url in host.point(at: url) }
        }
    }
}

/// Where the base URL lives. One place, because @AppStorage, the store host and
/// the verification harness (`defaults write … botnetBaseURL`) all have to name
/// the same key.
enum BaseURL {
    static let key = "botnetBaseURL"
    /// The port botnetd serves in production. A phone on the simulator shares
    /// the Mac's network stack, so loopback reaches a Mac-hosted daemon.
    static let fallback = "http://127.0.0.1:8730"

    static var stored: String {
        UserDefaults.standard.string(forKey: key) ?? fallback
    }
}

/// Owns the shared AppStore and rebuilds it when the base URL changes.
///
/// AppStore reads its address exactly once, when it builds its APIClient, from
/// the BOTNET_API environment variable — which the Mac app inherits from
/// whoever launched it. A phone has no launch environment to inherit, so the
/// app writes that variable itself before constructing the store. Rebuilding is
/// then the only way a URL typed in Settings can take effect, and it is also
/// the right semantics: a store caches one server's answers and nothing else.
@MainActor
final class StoreHost: ObservableObject {
    @Published private(set) var store: AppStore
    /// Bumped on every rebuild, so the screens can be given a fresh identity.
    @Published private(set) var generation = 0

    private var current: String

    init(baseURL: String) {
        Self.pointProcess(at: baseURL)
        current = baseURL
        store = AppStore()
    }

    func point(at url: String) {
        guard url != current else { return }
        Self.pointProcess(at: url)
        current = url
        store = AppStore()
        generation += 1
    }

    private static func pointProcess(at url: String) {
        setenv("BOTNET_API", url, 1)
    }
}
