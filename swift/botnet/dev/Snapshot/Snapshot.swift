// Snapshot renders the real sidebar and chat views to a PNG offscreen, so the
// UI can be reviewed and diffed without screen-recording permission and without
// a human driving the app.
//
//   ./dev/snapshot.sh                       # light + dark into build/snapshots
//   snapshot --calendar [--month]           # the Calendar panel, list or grid
//   snapshot --calendar --filter-calendar <name>  # with that calendar's chip active
//   snapshot --calendar --search <text>     # with the header search narrowing it
//   snapshot --event-sheet [--new-event]    # the event editor, rendered flat
//   snapshot --event-sheet --event-title <t>  # the editor on that exact event
//   snapshot --manage-calendars             # the calendar manager, rendered flat
//   snapshot --automation <name>            # that automation's pane
//   snapshot --automation <name> --disclose-run <status|index>
//                                           # with that run's detail disclosed
//   snapshot --project [--project-name <n>] # that project's pane
//   snapshot --project --add-fact [--fact-kind deadline|recurring|milestone|note]
//                                           # its Add Fact sheet, rendered flat
//   snapshot --project --edit               # its Edit Project sheet (Parent
//                                           # picker), rendered flat
//   snapshot --expand-projects <names>      # sidebar tree disclosed at those
//                                           # projects (comma-joined names)
//   snapshot --sidebar-search <text>        # with that typed in the sidebar's
//                                           # search, which force-reveals matches
//   snapshot --collapse <sections>          # sidebar sections rendered
//                                           # collapsed: services,automations,
//                                           # projects,bots
//
// It talks to whatever BOTNET_API points at, so point it at the demo server
// from dev/seed-demo.sh rather than the real ~/.botnet/net.db.

import AppKit
import SwiftUI

/// Which surface the capture puts beside the sidebar.
private enum Pane {
    case chat
    /// The filter is a calendar id (resolved from --filter-calendar's name),
    /// the query is --search's text: CalendarView opens with that chip active
    /// and that text typed — states a click can't produce offscreen.
    case calendar(CalendarMode, filter: String?, query: String)
    /// The event editor. The app presents it as a sheet; offscreen it is drawn
    /// flat, like BotDetails, because a sheet needs a real window to present.
    case eventSheet(EventTarget)
    /// The calendar manager, drawn flat for the same reason.
    case manageCalendars
    /// One automation's pane, optionally with a run's detail disclosed — a
    /// state a click can't produce offscreen.
    case automation(Automation, discloseRunID: String?)
    /// One project's pane, or one of its sheets drawn flat (a presented sheet
    /// needs a real window, same as the event editor).
    case project(Project, sheet: ProjectPaneSheet?)
}

/// Which of the project pane's sheets to draw flat beside the sidebar.
private enum ProjectPaneSheet {
    case addFact(FactKind)
    /// Edit Project, whose Parent picker is the one place the subtree-exclusion
    /// rule is visible — worth a render, not just a code read.
    case edit
}

@main
struct Snapshot {
    @MainActor
    static func main() async {
        let out = argument("--out") ?? "snapshot.png"
        let dark = CommandLine.arguments.contains("--dark")
        let details = CommandLine.arguments.contains("--details")
        // Renders the inspector with Memory collapsed (Tools stays open) so the
        // section hairline rule can be checked in a mixed open/closed state.
        let collapseMemory = CommandLine.arguments.contains("--collapse-memory")
        let width = Double(argument("--width") ?? "") ?? 1400
        let height = Double(argument("--height") ?? "") ?? 900

        _ = NSApplication.shared.setActivationPolicy(.prohibited)

        let store = AppStore()
        await store.refresh()
        // BotDetails fetches tools in a .task, but that would race the few
        // run-loop turns the capture allows; fetch before rendering instead.
        await store.loadTools()
        // Same for CalendarView's events: store.refresh() has already loaded
        // them, but the dependency belongs here where the capture can see it.
        await store.refreshEvents()
        // And for the sidebar's Automations section / the automation pane:
        // refresh() fetched the list; the pane's runs need the detail row too.
        await store.refreshAutomations()
        if let name = argument("--automation") {
            await store.loadAutomationDetail(name)
        }
        // And for the sidebar's Projects section / the project pane: the pane
        // loads its facts in a .task, which the capture window would race.
        await store.refreshProjects()

        guard let bot = store.bots.first else {
            fail("no bots at \(ProcessInfo.processInfo.environment["BOTNET_API"] ?? "the default port") — is the demo server running?")
        }
        await store.loadConversation(bot.id)

        let pane = pane(store)
        // The disclosed run's full row is a .task fetch in the app; awaited
        // here for the same capture-window reason as loadTools above.
        if case .automation(_, let runID?) = pane {
            await store.loadRunDetail(runID)
        }
        if case .project(let project, _) = pane {
            await store.loadProject(project.id)
        }

        render(store: store, bot: bot, pane: pane, dark: dark, details: details,
               collapseMemory: collapseMemory, collapsed: collapsedSections(),
               expandedProjects: expandedProjects(store),
               size: CGSize(width: width, height: height), to: out)
    }

    /// --expand-projects' names, resolved to ids. Names rather than ids because
    /// a project id is a ULID no one can type; loudly on no match, like
    /// --project-name, so a typo cannot pass review as a collapsed tree.
    @MainActor
    private static func expandedProjects(_ store: AppStore) -> Set<String>? {
        guard let raw = argument("--expand-projects") else { return nil }
        var ids = Set<String>()
        for part in raw.split(separator: ",") {
            let name = String(part).trimmingCharacters(in: .whitespaces)
            guard let match = store.projects.first(where: {
                $0.name.caseInsensitiveCompare(name) == .orderedSame
            }) else {
                fail("no project named \(name) on the server (--expand-projects wants names, comma-joined)")
            }
            ids.insert(match.id)
        }
        return ids
    }

    /// --collapse's sections, parsed strictly: a typo'd section name must not
    /// pass review as an expanded sidebar.
    private static func collapsedSections() -> Set<SidebarSection>? {
        guard let raw = argument("--collapse") else { return nil }
        var sections = Set<SidebarSection>()
        for part in raw.split(separator: ",") {
            guard let section = SidebarSection(rawValue: String(part)) else {
                fail("unknown sidebar section \(part) (want a comma-joined subset of services,automations,projects,bots)")
            }
            sections.insert(section)
        }
        return sections
    }

    @MainActor
    private static func pane(_ store: AppStore) -> Pane {
        let flags = CommandLine.arguments
        if flags.contains("--manage-calendars") { return .manageCalendars }
        // Loudly on both lookups, like --filter-calendar: a typo'd automation
        // or run selector must not pass review as some other pane.
        if let name = argument("--automation") {
            guard let automation = store.automations.first(where: { $0.name == name }) else {
                fail("no automation named \(name) on the server (is the bridge mounted?)")
            }
            var runID: String?
            if let selector = argument("--disclose-run") {
                let runs = automation.runs ?? []
                if let index = Int(selector), runs.indices.contains(index) {
                    runID = runs[index].id
                } else if let match = runs.first(where: { $0.status == selector }) {
                    runID = match.id
                } else {
                    fail("no run of \(name) matching \(selector) (want a status or a list index)")
                }
            }
            return .automation(automation, discloseRunID: runID)
        }
        // Loudly on no match, like --filter-calendar: a typo'd project name
        // must not pass review as whatever projects.first happened to be.
        if flags.contains("--project") {
            let project: Project
            if let name = argument("--project-name") {
                guard let match = store.projects.first(where: {
                    $0.name.caseInsensitiveCompare(name) == .orderedSame
                }) else {
                    fail("no project named \(name) on the server (has /v1/projects landed?)")
                }
                project = match
            } else {
                guard let first = store.projects.first else {
                    fail("no projects on the server (has /v1/projects landed?)")
                }
                project = first
            }
            if flags.contains("--edit") { return .project(project, sheet: .edit) }
            guard flags.contains("--add-fact") else { return .project(project, sheet: nil) }
            // Loudly again: a typo'd kind would otherwise render the deadline
            // form and pass review as whatever kind was asked for.
            guard let raw = argument("--fact-kind") else {
                return .project(project, sheet: .addFact(.deadline))
            }
            guard let kind = FactKind(rawValue: raw) else {
                fail("unknown fact kind \(raw) (want one of \(FactKind.allCases.map(\.rawValue).joined(separator: ",")))")
            }
            return .project(project, sheet: .addFact(kind))
        }
        guard flags.contains("--event-sheet") else {
            guard flags.contains("--calendar") else { return .chat }
            // Failing loudly beats a screenshot that silently shows "All":
            // a typo'd name would otherwise pass review as an unfiltered pane.
            var filter: String?
            if let name = argument("--filter-calendar") {
                guard let match = store.calendars.first(where: {
                    $0.name.caseInsensitiveCompare(name) == .orderedSame
                }) else {
                    fail("no calendar named \(name) on the demo server")
                }
                filter = match.id
            }
            return .calendar(flags.contains("--month") ? .month : .list,
                             filter: filter, query: argument("--search") ?? "")
        }
        // A named event fails loudly on no match, same as --filter-calendar:
        // a typo'd title must not pass review as whatever events.first was.
        if let title = argument("--event-title") {
            guard let match = store.events.first(where: {
                $0.title.caseInsensitiveCompare(title) == .orderedSame
            }) else {
                fail("no event titled \(title) on the demo server")
            }
            return .eventSheet(.existing(match))
        }
        // Editing an existing event is the interesting case; fall back to the
        // blank form when the calendar is empty rather than failing the run.
        guard !flags.contains("--new-event"), let event = store.events.first else {
            return .eventSheet(.new)
        }
        return .eventSheet(.existing(event))
    }

    @MainActor
    private static func render(store: AppStore, bot: Bot, pane: Pane, dark: Bool, details: Bool,
                               collapseMemory: Bool, collapsed: Set<SidebarSection>?,
                               expandedProjects: Set<String>?,
                               size: CGSize, to path: String) {
        let appearance = NSAppearance(named: dark ? .darkAqua : .aqua)!

        let selection: SidebarSelection = {
            switch pane {
            case .chat: return .bot(bot.id)
            case .automation(let automation, _): return .automation(automation.name)
            case .project(let project, _): return .project(project.id)
            default: return .service(.calendar)
            }
        }()

        let content = HStack(spacing: 0) {
            SidebarView(selection: .constant(selection), showNewBot: .constant(false),
                        showNewProject: .constant(false), collapsedOverride: collapsed,
                        expandedProjectsOverride: expandedProjects,
                        initialQuery: argument("--sidebar-search") ?? "")
                .frame(width: Metric.sidebarWidth)
            Rectangle().fill(Palette.hairline).frame(width: 1)
            switch pane {
            case .chat:
                ChatView(bot: bot, showDetails: .constant(details))
                // The real app presents BotDetails as an .inspector, which needs
                // a window toolbar to render; a plain third column previews the
                // same content offscreen.
                if details {
                    Rectangle().fill(Palette.hairline).frame(width: 1)
                    BotDetails(bot: bot, expanded: .constant(!collapseMemory))
                        .frame(width: 300)
                }
            case .calendar(let mode, let filter, let query):
                CalendarView(mode: .constant(mode), initialFilter: filter, initialQuery: query)
            case .automation(let automation, let runID):
                AutomationView(automation: automation, initialDisclosedRunID: runID)
            case .project(let project, let sheet):
                // The pane, or one of its sheets drawn flat at the sheet's own
                // size — a presented sheet needs a real window, same as the
                // event editor, and its toolbar (Cancel/Save) will not appear.
                switch sheet {
                case .addFact(let kind):
                    AddFactSheet(project: project, initialKind: kind)
                        .frame(width: 460, height: 400)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .background(Palette.chrome)
                case .edit:
                    EditProjectSheet(project: project)
                        .frame(width: 460, height: 320)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .background(Palette.chrome)
                case nil:
                    ProjectView(project: project)
                }
            case .eventSheet(let target):
                // Held at the sheet's own size on the pane's ground, so the
                // capture reads like the presented sheet rather than a form
                // stretched across the window. Its toolbar (Cancel/Save) needs
                // a real window and does not appear in a flat render.
                EventSheet(target: target)
                    .frame(width: 460, height: 440)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .background(Palette.chrome)
            case .manageCalendars:
                // Flat like the event sheet, and (also like it) without the
                // window toolbar, so the Done button is verified in code.
                ManageCalendarsSheet()
                    .frame(width: 460, height: 400)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .background(Palette.chrome)
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
