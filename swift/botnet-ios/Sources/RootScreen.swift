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
        .alert("Error", isPresented: Binding(
            get: { store.lastError != nil },
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
        case .project(let id): ProjectScreen(projectID: id, openAddFact: launch == .addFact)
        case .calendar: CalendarScreen()
        case .settings: SettingsScreen()
        }
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
        case .project, .addFact:
            if let project = store.projects.first { path = [.project(project.id)] }
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

    private var projectsSection: some View {
        Section {
            if store.projects.isEmpty {
                Text("No projects yet.")
                    .font(TypeScale.phoneRowPreview)
                    .foregroundStyle(Palette.secondaryText)
            }
            ForEach(store.projects) { project in
                NavigationLink(value: Route.project(project.id)) {
                    ProjectListRow(project: project)
                }
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

// One project in the list: the health dot the Mac's sidebar row wears, the
// name, and the one line that says what the project is for.
//
// Flat by design. The server may grow a `parentId` that would make this an
// indented tree; it is not on the Swift Project model today, so nothing here
// guesses at one — `indent` is the single place that would change when it
// lands.
private struct ProjectListRow: View {
    let project: Project
    var indent: CGFloat = 0

    var body: some View {
        HStack(spacing: Metric.phoneRowGap) {
            Circle()
                .fill(Palette.health(project.health))
                .frame(width: Metric.healthDot, height: Metric.healthDot)
            VStack(alignment: .leading, spacing: Metric.phoneTightGap) {
                Text(project.name)
                    .font(TypeScale.phoneRowTitle)
                    .foregroundStyle(Palette.primaryText)
                    .lineLimit(1)
                // The goal is what the project is about; with none, the next
                // due date is the only other thing worth a second line.
                if let subtitle {
                    Text(subtitle)
                        .font(TypeScale.phoneRowPreview)
                        .foregroundStyle(Palette.secondaryText)
                        .lineLimit(1)
                }
            }
            Spacer(minLength: 0)
            Text(factCount)
                .font(TypeScale.phoneRowMeta)
                .foregroundStyle(Palette.secondaryText)
        }
        .padding(.leading, indent)
        .frame(minHeight: Metric.phoneTapTarget)
    }

    private var subtitle: String? {
        project.hasGoal ? project.goal : project.nextDueText
    }

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
