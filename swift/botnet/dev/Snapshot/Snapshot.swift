// Snapshot renders the real sidebar and chat views to a PNG offscreen, so the
// UI can be reviewed and diffed without screen-recording permission and without
// a human driving the app.
//
//   ./dev/snapshot.sh                       # light + dark into build/snapshots
//
// It talks to whatever BOTNET_API points at, so point it at the demo server
// from dev/seed-demo.sh rather than the real ~/.botnet/net.db.

import AppKit
import SwiftUI

@main
struct Snapshot {
    @MainActor
    static func main() async {
        let out = argument("--out") ?? "snapshot.png"
        let dark = CommandLine.arguments.contains("--dark")
        let details = CommandLine.arguments.contains("--details")
        let width = Double(argument("--width") ?? "") ?? 1400
        let height = Double(argument("--height") ?? "") ?? 900

        _ = NSApplication.shared.setActivationPolicy(.prohibited)

        let store = AppStore()
        await store.refresh()
        // BotDetails fetches tools in a .task, but that would race the few
        // run-loop turns the capture allows; fetch before rendering instead.
        await store.loadTools()

        guard let bot = store.bots.first else {
            fail("no bots at \(ProcessInfo.processInfo.environment["BOTNET_API"] ?? "the default port") — is the demo server running?")
        }
        await store.loadConversation(bot.id)

        render(store: store, bot: bot, dark: dark, details: details,
               size: CGSize(width: width, height: height), to: out)
    }

    @MainActor
    private static func render(store: AppStore, bot: Bot, dark: Bool, details: Bool, size: CGSize, to path: String) {
        let appearance = NSAppearance(named: dark ? .darkAqua : .aqua)!

        let content = HStack(spacing: 0) {
            SidebarView(selectedBotID: .constant(bot.id), showNewBot: .constant(false))
                .frame(width: Metric.sidebarWidth)
            Rectangle().fill(Palette.hairline).frame(width: 1)
            ChatView(bot: bot, showDetails: .constant(details))
            // The real app presents BotDetails as an .inspector, which needs a
            // window toolbar to render; a plain third column previews the same
            // content offscreen.
            if details {
                Rectangle().fill(Palette.hairline).frame(width: 1)
                BotDetails(bot: bot, expanded: .constant(true))
                    .frame(width: 300)
            }
        }
        .environmentObject(store)
        .environment(\.colorScheme, dark ? .dark : .light)
        .frame(width: size.width, height: size.height)
        .background(Palette.chrome)

        // ImageRenderer draws ScrollView and TextField as blank or as a
        // "not available" glyph, which is most of this UI. Hosting the view in
        // a real (offscreen) window and caching its display gives AppKit the
        // layout pass those need.
        let hosting = NSHostingView(rootView: content)
        hosting.frame = CGRect(origin: .zero, size: size)
        let window = NSWindow(
            contentRect: hosting.frame,
            styleMask: [.borderless],
            backing: .buffered,
            defer: false
        )
        window.appearance = appearance
        window.contentView = hosting
        window.orderBack(nil)

        // Palette resolves through NSColor dynamic providers, which read the
        // current drawing appearance rather than the SwiftUI environment, so the
        // capture has to happen with that appearance made current.
        var encoded: Data?
        appearance.performAsCurrentDrawingAppearance {
            hosting.layoutSubtreeIfNeeded()
            // SwiftUI settles its layout over a few run-loop turns; without this
            // the scrollable content is captured before it has any.
            for _ in 0..<8 {
                RunLoop.current.run(mode: .default, before: Date(timeIntervalSinceNow: 0.05))
            }
            hosting.layoutSubtreeIfNeeded()

            guard let bitmap = hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds) else { return }
            hosting.cacheDisplay(in: hosting.bounds, to: bitmap)
            encoded = bitmap.representation(using: .png, properties: [:])
        }
        guard let png = encoded else {
            fail("could not capture \(Int(size.width))x\(Int(size.height))")
        }
        do {
            try png.write(to: URL(fileURLWithPath: path))
        } catch {
            fail("write \(path): \(error.localizedDescription)")
        }
        print("wrote \(path)  \(Int(size.width))x\(Int(size.height))  \(dark ? "dark" : "light")")
    }

    private static func argument(_ flag: String) -> String? {
        guard let i = CommandLine.arguments.firstIndex(of: flag),
              i + 1 < CommandLine.arguments.count else { return nil }
        return CommandLine.arguments[i + 1]
    }

    private static func fail(_ message: String) -> Never {
        FileHandle.standardError.write(Data("snapshot: \(message)\n".utf8))
        exit(1)
    }
}
