// RootScreen.swift — the phone's one list: the projects, the bots, and the
// calendar, in that order, with everything else hanging off it as a pushed
// screen. The Mac's sidebar is a permanent column beside its detail pane; a
// phone has one column, so the same three groups become a list you leave and
// come back to.
//
// The server owns all of it. This screen fetches on appear, refetches on pull,
// and carries the app's ONE error alert — every store call funnels its failure
// into `lastError`, exactly as ContentView does on the Mac.

import SwiftUI

/// Where the stack can be. A typed route, mirroring the Mac's SidebarSelection:
/// a destination is named by an id, never by a captured model copy, so every
/// screen re-resolves its own row from the store as it renders and a refetch
/// that renamed or removed it reaches the open screen.
enum Route: Hashable {
    case bot(String)
    case project(String)
    case calendar
    case settings
}

struct RootScreen: View {
    @EnvironmentObject var store: AppStore

    @State private var path: [Route] = []
    @State private var showNewProject = false
    /// Read once, at launch, and applied after the first fetch — the screens it
    /// names are chosen from rows the server actually returned.
    @State private var launch = LaunchScreen.requested
    @State private var collapsedProjects: Set<String> = []

    var body: some View {
        NavigationStack(path: $path) {
            List {
                if !store.serverReachable { unreachableSection }
                // Absent entirely — header and "+" too — when the server has no
                // /v1/projects. An old botnetd must render exactly as it did
                // before projects existed, and the header carries the only
                // create affordance, so it cannot exist where the POST 404s.
                if !store.projectsUnavailable { projectsSection }
                botsSection
                calendarSection
            }
            .navigationTitle("BotNet")
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button { path.append(.settings) } label: {
                        Image(systemName: "gearshape")
                    }
                    .accessibilityLabel("Settings")
                }
            }
            .refreshable { await store.refresh() }
            .task {
                await store.refresh()
                openRequestedScreen()
            }
            .sheet(isPresented: $showNewProject) { NewProjectSheet() }
            .navigationDestination(for: Route.self) { route in
                destination(route)
            }
        }
        // One alert for the whole app, as on the Mac: every store call reports
        // its failure through lastError and nothing else presents an error.
        //
        // Except unreachability. The store sets lastError AND serverReachable
        // on the same failure, so a phone pointed at a dead address opened onto
        // a modal it had to dismiss before it could read the banner that says
        // the same thing and offers the fix. The banner wins; the alert is for
        // what fails while the server IS answering.
        .alert("Error", isPresented: Binding(
            get: { store.lastError != nil && store.serverReachable },
            set: { if !$0 { store.lastError = nil } }
        )) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(store.lastError ?? "")
        }
    }

    @ViewBuilder
    private func destination(_ route: Route) -> some View {
        switch route {
        case .bot(let id): ChatScreen(botID: id)
        case .project(let id):
            ProjectScreen(projectID: id,
                          openAddFact: launch?.opensAddFact ?? false,
                          addFactKind: launch?.factKind ?? .deadline)
        case .calendar: CalendarScreen()
        case .settings: SettingsScreen()
        }
    }

    private func toggle(_ id: String) {
        if collapsedProjects.remove(id) == nil { collapsedProjects.insert(id) }
    }

    // The launch flag names a screen, never data: the bot and the project are
    // whichever ones the server put first, and a run with no rows opens nothing.
    private func openRequestedScreen() {
        guard let launch else { return }
        switch launch {
        case .settings: path = [.settings]
        case .calendar: path = [.calendar]
        case .chat:
            if let bot = store.bots.first { path = [.bot(bot.id)] }
        case .project, .addFact, .addFactRecurring:
            // The first ROOT, which is the first row the list draws — the flat
            // list's own first entry can be a child, whose parent is what the
            // screen above it shows.
            if let project = tree.roots.first { path = [.project(project.id)] }
        }
    }

    // Unreachable is not an error the alert should carry: it persists, and the
    // one thing that fixes it is the address in Settings. So it states itself
    // at the top of the list and offers exactly that.
    private var unreachableSection: some View {
        Section {
            Button { path.append(.settings) } label: {
                HStack(spacing: Metric.phoneRowGap) {
                    Image(systemName: "exclamationmark.triangle.fill")
                        .foregroundStyle(Palette.attention)
                    VStack(alignment: .leading, spacing: Metric.phoneTightGap) {
                        Text("botnetd isn't answering")
                            .font(TypeScale.phoneRowTitle)
                            .foregroundStyle(Palette.primaryText)
                        Text("Check the server address.")
                            .font(TypeScale.phoneRowPreview)
                            .foregroundStyle(Palette.secondaryText)
                    }
                    Spacer(minLength: 0)
                    Image(systemName: "chevron.right")
                        .font(TypeScale.phoneRowMeta)
                        .foregroundStyle(Palette.secondaryText)
                }
                .frame(minHeight: Metric.phoneTapTarget)
            }
        }
    }

    /// The tree the rows are drawn from, built from the flat list the server
    /// sends. The shared ProjectTree is the ONE thing that knows the shape —
    /// orphans render as roots, parent chains are cycle-guarded — so the phone
    /// walks no parentId of its own.
    private var tree: ProjectTree { ProjectTree(store.projects) }

    /// Which parents the reader has CLOSED, not which are open. A phone's list
    /// is the only view of the tree it has, and a child hidden behind a chevron
    /// nobody opened is work that is simply never seen — so a parent discloses
    /// by default and staying closed is the deliberate act.
    private var expandedProjects: Set<String> {
        Set(store.projects.map(\.id)).subtracting(collapsedProjects)
    }

    private var projectsSection: some View {
        Section {
            // "No projects yet" is only true of a server that answered. With
            // none answering, the banner above already says why the list is
            // bare, and this line would be the app inventing a fact.
            if store.projects.isEmpty, store.serverReachable {
                Text("No projects yet.")
                    .font(TypeScale.phoneRowPreview)
                    .foregroundStyle(Palette.secondaryText)
            }
            ForEach(tree.rows(expanded: expandedProjects)) { row in
                ProjectListRow(row: row) { toggle(row.project.id) }
            }
        } header: {
            HStack {
                Text("Projects")
                Spacer()
                Button { showNewProject = true } label: {
                    Image(systemName: "plus")
                }
                .accessibilityLabel("New project")
            }
        }
    }

    private var botsSection: some View {
        Section("Bots") {
            ForEach(store.bots) { bot in
                NavigationLink(value: Route.bot(bot.id)) {
                    BotListRow(
                        bot: bot,
                        preview: store.preview(for: bot),
                        stamp: store.lastActivity(for: bot)
                    )
                }
            }
        }
    }

    private var calendarSection: some View {
        Section {
            NavigationLink(value: Route.calendar) {
                HStack(spacing: Metric.phoneRowGap) {
                    Image(systemName: "calendar")
                        .foregroundStyle(Palette.primaryText)
                    Text("Calendar")
                        .font(TypeScale.phoneRowTitle)
                        .foregroundStyle(Palette.primaryText)
                    Spacer(minLength: 0)
                    Text(upcomingCount)
                        .font(TypeScale.phoneRowMeta)
                        .foregroundStyle(Palette.secondaryText)
                }
                .frame(minHeight: Metric.phoneTapTarget)
            }
        }
    }

    /// What is still ahead, counted the way the calendar itself splits the
    /// list: by local day, so an event earlier today still counts as upcoming.
    private var upcomingCount: String {
        let today = Calendar.current.startOfDay(for: Date())
        let count = store.instances.filter { $0.day >= today }.count
        return count == 1 ? "1 upcoming" : "\(count) upcoming"
    }
}

// One line of the project tree: a disclosure caret where there are children,
// the SEVERITY dot, the name, and the one line saying what the project is for.
//
// The caret sits outside the NavigationLink on purpose — inside it, a tap on
// the caret would push the project rather than open its children.
private struct ProjectListRow: View {
    let row: ProjectTree.Row
    let toggle: () -> Void

    private var project: Project { row.project }

    var body: some View {
        HStack(spacing: 0) {
            Color.clear.frame(width: CGFloat(row.depth) * Metric.phoneTreeIndent)
            caret
            NavigationLink(value: Route.project(project.id)) {
                content
            }
        }
    }

    // A fixed column whether or not this project has children, so every name in
    // the list starts at one x for its depth.
    @ViewBuilder
    private var caret: some View {
        if row.hasChildren {
            Button(action: toggle) {
                Image(systemName: "chevron.right")
                    .font(TypeScale.phoneRowMeta)
                    .foregroundStyle(Palette.secondaryText)
                    .rotationEffect(.degrees(row.expanded ? 90 : 0))
                    .frame(width: Metric.phoneCaretWidth, height: Metric.phoneTapTarget)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(row.expanded ? "Collapse \(project.name)" : "Expand \(project.name)")
        } else {
            Color.clear.frame(width: Metric.phoneCaretWidth)
        }
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: Metric.phoneTightGap) {
            HStack(spacing: Metric.phoneRowGap) {
                // Severity is the scale the tree reads by; projectDot falls back
                // to health in one place, for a botnetd that derives none. It
                // sits ON the title line rather than centred over both, or a
                // two-line row parks its dot between the lines it marks.
                Circle()
                    .fill(Palette.projectDot(severity: project.severity, health: project.health))
                    .frame(width: Metric.healthDot, height: Metric.healthDot)
                Text(project.name)
                    .font(TypeScale.phoneRowTitle)
                    .foregroundStyle(Palette.primaryText)
                    .lineLimit(1)
                Spacer(minLength: Metric.phoneRowGap)
                Text(factCount)
                    .font(TypeScale.phoneRowMeta)
                    .foregroundStyle(Palette.secondaryText)
            }
            // The urgency reading, on every row. The goal lives on the project's
            // own screen: a phone list is opened to find what needs doing now,
            // and a row that spends its second line on prose can't answer that.
            Text(project.severityText)
                .font(TypeScale.phoneRowPreview)
                .foregroundStyle(Palette.secondaryText)
                .lineLimit(1)
                .padding(.leading, Metric.healthDot + Metric.phoneRowGap)
        }
        .frame(minHeight: Metric.phoneTapTarget)
    }

    // This project's OWN facts. A parent's count deliberately excludes its
    // children's, which is what makes the number readable on a parent row.
    private var factCount: String {
        project.factCount == 1 ? "1 fact" : "\(project.factCount) facts"
    }
}

// One bot in the list: face, name, the last thing said, and when. The unread
// pip is the only loud thing on the row, for the same reason it is on the Mac.
private struct BotListRow: View {
    let bot: Bot
    let preview: String?
    let stamp: Date?

    var body: some View {
        HStack(spacing: Metric.phoneRowGap) {
            BotAvatar(botID: bot.id, size: Metric.phoneAvatar)
            VStack(alignment: .leading, spacing: Metric.phoneTightGap) {
                Text(bot.displayName)
                    .font(TypeScale.phoneRowTitle)
                    .foregroundStyle(Palette.primaryText)
                    .lineLimit(1)
                if let preview {
                    Text(Self.oneLine(preview))
                        .font(TypeScale.phoneRowPreview)
                        .foregroundStyle(Palette.secondaryText)
                        .lineLimit(1)
                        .truncationMode(.tail)
                }
            }
            Spacer(minLength: 0)
            VStack(alignment: .trailing, spacing: Metric.phoneTightGap) {
                if let stamp {
                    Text(Self.stamped(stamp))
                        .font(TypeScale.phoneRowMeta)
                        .foregroundStyle(Palette.secondaryText)
                }
                if bot.hasUnread {
                    Circle()
                        .fill(Palette.attention)
                        .frame(width: Metric.phoneUnreadDot, height: Metric.phoneUnreadDot)
                }
            }
        }
        .frame(minHeight: Metric.phoneTapTarget)
    }

    // Paragraph breaks would otherwise survive as runs of blank space on a row
    // that only ever shows one line.
    private static func oneLine(_ content: String) -> String {
        content
            .components(separatedBy: .whitespacesAndNewlines)
            .filter { !$0.isEmpty }
            .joined(separator: " ")
    }

    private static func stamped(_ date: Date) -> String {
        if Calendar.current.isDateInToday(date) {
            return date.formatted(date: .omitted, time: .shortened)
        }
        return date.formatted(.dateTime.month(.defaultDigits).day())
    }
}

/// What a screen shows when the row it was pushed for is gone: a bot deleted
/// from the Mac, a project a bot's tool removed. The route holds an id, not a
/// copy, so this is the honest answer rather than stale data drawn from a
/// captured struct.
struct VanishedRow: View {
    let noun: String

    var body: some View {
        ContentUnavailableView {
            Label("No longer there", systemImage: "questionmark.circle")
        } description: {
            Text("This \(noun) isn't on the server any more.")
        }
    }
}
