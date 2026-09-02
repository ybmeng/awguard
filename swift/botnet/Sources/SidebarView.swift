// SidebarView.swift — the sidebar: search pinned on top, then an explorer
// tree of collapsible sections (Services, Automations, Bots) the way VS
// Code's Explorer stacks folders. Lives apart from BotNetApp.swift so
// dev/Snapshot can render it without pulling in the @main app.

import SwiftUI

/// What the sidebar has selected. A service is not a bot and has no bot id, so
/// the two are separate cases rather than sentinel strings in a bot id — the
/// detail pane switches on the case and never has to know which ids are real.
/// An automation is keyed by its name: that IS its server-side identity (the
/// registry has no other id). A project, unlike an automation, does have an id
/// and can be renamed, so it is keyed by id.
enum SidebarSelection: Hashable {
    case bot(String)
    case service(ServiceKind)
    case automation(String)
    case project(String)

    var botID: String? {
        guard case .bot(let id) = self else { return nil }
        return id
    }

    var service: ServiceKind? {
        guard case .service(let kind) = self else { return nil }
        return kind
    }

    var automationName: String? {
        guard case .automation(let name) = self else { return nil }
        return name
    }

    var projectID: String? {
        guard case .project(let id) = self else { return nil }
        return id
    }
}

/// The sidebar's collapsible sections, in render order. The rawValues are the
/// vocabulary of Snapshot's --collapse flag, and each section's expansion
/// persists under its own @AppStorage key so future sections slot in like
/// folders.
enum SidebarSection: String, CaseIterable {
    case services, automations, projects, bots

    var title: String { rawValue.capitalized }
}

/// The persistently running services the bots share with the user. Calendar is
/// the first with a surface of its own; std_artifacts still lists — the service
/// exists — but has no panel yet, so its row does not select.
enum ServiceKind: String, CaseIterable, Identifiable, Hashable {
    case calendar
    case artifacts

    var id: String { rawValue }

    var title: String {
        switch self {
        case .calendar: return "Calendar"
        case .artifacts: return "std_artifacts"
        }
    }

    var symbol: String {
        switch self {
        case .calendar: return "calendar"
        case .artifacts: return "externaldrive"
        }
    }

    var hasSurface: Bool { self == .calendar }
}

struct SidebarView: View {
    @EnvironmentObject var store: AppStore
    @Binding var selection: SidebarSelection?
    @Binding var showNewBot: Bool
    @Binding var showNewProject: Bool
    /// Snapshot-only: render these sections collapsed regardless of the
    /// persisted state, so an offscreen render is deterministic and leaves no
    /// defaults litter behind. Nil in the app.
    var collapsedOverride: Set<SidebarSection>? = nil
    /// Snapshot-only, same reasoning: which projects render disclosed. Nil in
    /// the app, where the persisted set below decides.
    var expandedProjectsOverride: Set<String>? = nil

    @State private var query: String

    /// `initialQuery` is Snapshot-only, the same seam CalendarView uses: the
    /// search field cannot be typed into offscreen, and force-revealed matches
    /// are a rendered state worth proving.
    init(selection: Binding<SidebarSelection?>, showNewBot: Binding<Bool>,
         showNewProject: Binding<Bool>, collapsedOverride: Set<SidebarSection>? = nil,
         expandedProjectsOverride: Set<String>? = nil, initialQuery: String = "") {
        _selection = selection
        _showNewBot = showNewBot
        _showNewProject = showNewProject
        self.collapsedOverride = collapsedOverride
        self.expandedProjectsOverride = expandedProjectsOverride
        _query = State(initialValue: initialQuery)
    }

    // One key per section (not one dictionary) so a future section adds a
    // line here and nothing migrates. Defaults expanded: a fresh install
    // should show everything it has.
    @AppStorage("sidebar.servicesExpanded") private var servicesExpanded = true
    @AppStorage("sidebar.automationsExpanded") private var automationsExpanded = true
    @AppStorage("sidebar.projectsExpanded") private var projectsExpanded = true
    @AppStorage("sidebar.botsExpanded") private var botsExpanded = true

    // Which projects are disclosed, as ONE comma-joined string rather than a
    // key per project: the set is unbounded and its members come and go, so a
    // key each would litter defaults with every project ever expanded. Sorted
    // on write so the stored value is stable to diff.
    @AppStorage("sidebar.expandedProjects") private var expandedProjectIDs = ""

    private var searching: Bool {
        !query.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    private var expandedProjects: Set<String> {
        if let expandedProjectsOverride { return expandedProjectsOverride }
        return Set(expandedProjectIDs.split(separator: ",").map(String.init))
    }

    /// The whole Projects section's shape, from the ONE place that knows it.
    /// The rows, the section's search-reveal and the empty state all read this,
    /// so none of them walks parentId itself.
    private var projectRowSpecs: [ProjectTree.Row] {
        ProjectTree(store.projects).rows(expanded: expandedProjects, query: query)
    }

    private func toggleProject(_ id: String) {
        // An override is a fixed render, not a state a click may edit — and
        // writing here would put defaults litter back into the snapshot path.
        guard expandedProjectsOverride == nil else { return }
        var ids = expandedProjects
        if !ids.insert(id).inserted { ids.remove(id) }
        expandedProjectIDs = ids.sorted().joined(separator: ",")
    }

    private var visibleBots: [Bot] {
        let needle = query.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !needle.isEmpty else { return store.bots }
        return store.bots.filter { $0.displayName.range(of: needle, options: .caseInsensitive) != nil }
    }

    private func expansion(_ section: SidebarSection) -> Binding<Bool> {
        if let collapsedOverride {
            return .constant(!collapsedOverride.contains(section))
        }
        switch section {
        case .services: return $servicesExpanded
        case .automations: return $automationsExpanded
        case .projects: return $projectsExpanded
        case .bots: return $botsExpanded
        }
    }

    var body: some View {
        VStack(spacing: 0) {
            searchRow
            Rectangle().fill(Palette.hairline).frame(height: 1)
            ScrollView {
                VStack(alignment: .leading, spacing: 2) {
                    section(.services) { serviceRows }
                    // Absent entirely — header too — when the server has no
                    // automations routes (standalone botnetd, old server).
                    if !store.automationsUnavailable {
                        section(.automations) { automationRows }
                    }
                    // Absent the same way when the server has no /v1/projects.
                    // The header carries the only New Project affordance, so it
                    // cannot exist on a server that would 404 the create.
                    if !store.projectsUnavailable {
                        section(.projects) {
                            Button { showNewProject = true } label: {
                                Image(systemName: "plus")
                                    .font(TypeScale.sectionChevron)
                                    .foregroundStyle(Palette.secondaryText)
                            }
                            .buttonStyle(.borderless)
                            .help("Create a project")
                        } rows: {
                            projectRows
                        }
                    }
                    // Searching reveals matches through a collapsed Bots
                    // section (revealed: below); the persisted choice is
                    // untouched and comes back when the search clears.
                    section(.bots) { botRows }
                }
                .padding(Metric.sidebarGutter)
            }
            .scrollContentBackground(.hidden)
            if !store.serverReachable {
                Label("botnetd not reachable", systemImage: "exclamationmark.triangle")
                    .font(TypeScale.rowMeta)
                    .foregroundStyle(Palette.attention)
                    .padding(.horizontal, Metric.sidebarGutter * 2)
                    .padding(.bottom, Metric.sidebarGutter)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .background(Palette.chrome)
    }

    /// Whether a section's rows draw. A search reveals the two sections it
    /// actually searches — Bots and Projects — so a match is visible even under
    /// a collapsed header; the chevron turns with it, honestly, and the
    /// persisted choice comes back when the search clears. Projects reveals
    /// only when the query MATCHES something, so an unrelated search does not
    /// pop an empty section open.
    private func revealed(_ section: SidebarSection) -> Bool {
        if expansion(section).wrappedValue { return true }
        guard searching else { return false }
        switch section {
        case .bots: return true
        case .projects: return !projectRowSpecs.isEmpty
        case .services, .automations: return false
        }
    }

    @ViewBuilder
    private func section<Rows: View>(
        _ section: SidebarSection, @ViewBuilder rows: () -> Rows
    ) -> some View {
        self.section(section, accessory: { EmptyView() }, rows: rows)
    }

    /// The accessory sits in the header's trailing slot (the "+" that creates a
    /// project), beside the collapse button rather than inside it — a button
    /// nested in another button's label swallows its own clicks.
    @ViewBuilder
    private func section<Accessory: View, Rows: View>(
        _ section: SidebarSection,
        @ViewBuilder accessory: @escaping () -> Accessory,
        @ViewBuilder rows: () -> Rows
    ) -> some View {
        SectionHeader(
            title: section.title,
            expanded: expansion(section),
            revealed: revealed(section),
            accessory: accessory
        )
        if revealed(section) {
            VStack(alignment: .leading, spacing: 2) { rows() }
                .padding(.leading, Metric.sidebarIndent)
                .padding(.bottom, 6)
        }
    }

    @ViewBuilder
    private var serviceRows: some View {
        ForEach(ServiceKind.allCases) { kind in
            ServiceRow(kind: kind, selected: selection?.service == kind) {
                selection = .service(kind)
            }
        }
    }

    @ViewBuilder
    private var automationRows: some View {
        ForEach(store.automations) { automation in
            AutomationRow(
                automation: automation,
                selected: selection?.automationName == automation.name
            ) {
                selection = .automation(automation.name)
            }
        }
    }

    @ViewBuilder
    private var projectRows: some View {
        let rows = projectRowSpecs
        if store.projects.isEmpty {
            placeholder("No projects yet.")
        } else if rows.isEmpty {
            // Only reachable while searching: the section has projects, none of
            // them match. Saying so beats an expanded header over nothing.
            placeholder("No projects match.")
        }
        ForEach(rows) { row in
            ProjectRow(
                row: row,
                selected: selection?.projectID == row.project.id,
                select: { selection = .project(row.project.id) },
                toggle: { toggleProject(row.project.id) }
            )
        }
    }

    private func placeholder(_ text: String) -> some View {
        Text(text)
            .font(TypeScale.rowMeta)
            .foregroundStyle(Palette.secondaryText)
            .padding(.vertical, Metric.rowVPad)
            .padding(.horizontal, Metric.sidebarGutter)
    }

    @ViewBuilder
    private var botRows: some View {
        ForEach(visibleBots) { bot in
            BotRow(
                bot: bot,
                preview: store.preview(for: bot),
                stamp: store.lastActivity(for: bot),
                fallback: modelName(bot.model),
                selected: selection?.botID == bot.id
            ) {
                selection = .bot(bot.id)
                Task { await store.markRead(bot) }
            }
            .contextMenu {
                Button("Delete", role: .destructive) {
                    Task { await store.deleteBot(bot) }
                }
            }
        }
    }

    private var searchRow: some View {
        HStack(spacing: 6) {
            HStack(spacing: 6) {
                Image(systemName: "magnifyingglass")
                    .foregroundStyle(Palette.secondaryText)
                TextField("Search", text: $query)
                    .textFieldStyle(.plain)
                    .font(TypeScale.rowPreview)
                    .foregroundStyle(Palette.primaryText)
            }
            .padding(.horizontal, 8)
            .padding(.vertical, 6)
            .background(
                Palette.fieldFill,
                in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
            )

            Button { showNewBot = true } label: {
                Image(systemName: "plus").foregroundStyle(Palette.secondaryText)
            }
            .buttonStyle(.borderless)
            .help("Create a bot")
        }
        .padding(Metric.sidebarGutter)
    }

    private func modelName(_ id: String) -> String {
        store.models.first(where: { $0.id == id })?.name ?? id
    }
}

// A section's chevron header row — caret plus label, the Explorer idiom.
// `revealed` is what the caret shows (a search can reveal a collapsed Bots
// section); clicking still toggles the persisted `expanded` choice.
// The optional trailing accessory follows InspectorSection's shape: a sibling
// of the collapse button, not part of its label, so its own clicks land.
private struct SectionHeader<Accessory: View>: View {
    let title: String
    @Binding var expanded: Bool
    let revealed: Bool
    @ViewBuilder let accessory: () -> Accessory

    @State private var hovering = false

    var body: some View {
        HStack(spacing: 3) {
            Button { expanded.toggle() } label: {
                HStack(spacing: 3) {
                    Image(systemName: "chevron.right")
                        .font(TypeScale.sectionChevron)
                        .rotationEffect(.degrees(revealed ? 90 : 0))
                        .frame(width: Metric.sectionChevronWidth)
                    Text(title)
                        .font(TypeScale.sectionLabel)
                    Spacer(minLength: 0)
                }
                .foregroundStyle(Palette.secondaryText)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help(revealed ? "Collapse \(title)" : "Expand \(title)")
            accessory()
        }
        .padding(.vertical, 4)
        .padding(.horizontal, 4)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            hovering ? Palette.rowHover : .clear,
            in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
        )
        .onHover { hovering = $0 }
    }
}

// An automation's sidebar row: freshness dot in the shared leading column,
// then the name. Built like ServiceRow, and like it deliberately not shared
// with BotRow — the rows agree on hover, selection and the leading column
// width, and on nothing else.
private struct AutomationRow: View {
    let automation: Automation
    let selected: Bool
    let select: () -> Void

    @State private var hovering = false

    var body: some View {
        Button(action: select) {
            HStack(spacing: 8) {
                Circle()
                    .fill(Palette.freshness(automation.freshness))
                    .frame(width: Metric.freshnessDot, height: Metric.freshnessDot)
                    // The same leading width a bot's avatar takes, so
                    // automation names share the sidebar's one text column.
                    .frame(width: Metric.avatarRow)
                Text(automation.name)
                    .font(TypeScale.rowTitle)
                    .lineLimit(1)
                Spacer(minLength: 0)
            }
            .foregroundStyle(Palette.primaryText)
            .padding(.vertical, Metric.rowVPad)
            .padding(.horizontal, Metric.sidebarGutter)
            .frame(maxWidth: .infinity, alignment: .leading)
            .contentShape(RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous))
            .background(
                background,
                in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
            )
        }
        .buttonStyle(.plain)
        .onHover { hovering = $0 }
        .help("\(automation.name) — \(automation.freshness)")
        .contextMenu {
            // Hidden, not disabled, without a wire path: a service that
            // predates the bridge's path field has nothing to open.
            if let path = automation.path {
                Button("Open in Cursor") {
                    // The Finder window opening IS the fallback feedback out
                    // here; the pane's button is where the notice lives.
                    Task { FolderOpener.openInCursor(path) }
                }
                Button("Reveal in Finder") {
                    FolderOpener.reveal(path)
                }
            }
        }
    }

    private var background: Color {
        if selected { return Palette.rowSelected }
        return hovering ? Palette.rowHover : .clear
    }
}

// A project's sidebar row, one node of the tree: an indent per level, a
// disclosure caret where there are children, the SEVERITY dot, the name, and
// how long until the next thing is due. The row is handed a ProjectTree.Row and
// never walks parentId itself — depth and disclosure are decided in one place.
//
// The caret is a SIBLING of the select button, not inside its label: a button
// nested in another button's label swallows its own clicks, the same trap
// SectionHeader's accessory slot works around.
private struct ProjectRow: View {
    let row: ProjectTree.Row
    let selected: Bool
    let select: () -> Void
    let toggle: () -> Void

    @State private var hovering = false

    private var project: Project { row.project }

    var body: some View {
        HStack(spacing: 4) {
            Color.clear
                .frame(width: CGFloat(row.depth) * Metric.projectTreeIndent, height: 0)
            disclosure
            Button(action: select) { content }
                .buttonStyle(.plain)
        }
        .padding(.vertical, Metric.rowVPad)
        .padding(.horizontal, Metric.sidebarGutter)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            background,
            in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
        )
        .onHover { hovering = $0 }
        // The severity is in the tooltip because the dot can only color it, and
        // "S0" is the word the tool and the pane both use for this row.
        .help("\(project.name) — \(project.severityText)")
    }

    // A leaf reserves the caret's width and draws nothing, so sibling names
    // line up whether or not they have children.
    @ViewBuilder
    private var disclosure: some View {
        if row.hasChildren {
            Button(action: toggle) {
                Image(systemName: "chevron.right")
                    .font(TypeScale.sectionChevron)
                    .foregroundStyle(Palette.secondaryText)
                    .rotationEffect(.degrees(row.expanded ? 90 : 0))
                    .frame(width: Metric.projectDisclosureWidth)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help(row.expanded ? "Collapse \(project.name)" : "Expand \(project.name)")
        } else {
            Color.clear.frame(width: Metric.projectDisclosureWidth, height: 1)
        }
    }

    private var content: some View {
        HStack(spacing: 8) {
            Circle()
                .fill(Palette.projectDot(severity: project.severity, health: project.health))
                .frame(width: Metric.healthDot, height: Metric.healthDot)
                // A compact dot column rather than the bot avatar's width: the
                // caret column now does the aligning, and three levels of
                // 30pt-plus leading would push a deep name off the row.
                .frame(width: Metric.projectDisclosureWidth)
            Text(project.name)
                .font(TypeScale.rowTitle)
                .lineLimit(1)
            Spacer(minLength: 0)
            if let due = project.nextDueText {
                Text(due)
                    .font(TypeScale.rowMeta)
                    // Overdue is the one state the list itself has to
                    // shout; the rest stay quiet meta text.
                    .foregroundStyle(project.health == "overdue"
                                     ? Palette.healthOverdue : Palette.secondaryText)
                    .lineLimit(1)
            }
        }
        .foregroundStyle(Palette.primaryText)
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
    }

    private var background: Color {
        if selected { return Palette.rowSelected }
        return hovering ? Palette.rowHover : .clear
    }
}

// Deliberately built like BotRow rather than shared with it: the two rows agree
// on hover, selection and the leading column width, and on nothing else — a bot
// row carries a preview, a stamp and unread state a service has no analogue for.
private struct ServiceRow: View {
    let kind: ServiceKind
    let selected: Bool
    let select: () -> Void

    @State private var hovering = false

    private var live: Bool { kind.hasSurface }

    var body: some View {
        Button { if live { select() } } label: {
            HStack(spacing: 8) {
                Image(systemName: kind.symbol)
                    .font(TypeScale.serviceIcon)
                    // The same leading width a bot's avatar takes, so service
                    // titles and bot names share one text column.
                    .frame(width: Metric.avatarRow)
                Text(kind.title)
                    .font(TypeScale.rowTitle)
                    .lineLimit(1)
                Spacer(minLength: 0)
            }
            .foregroundStyle(live ? Palette.primaryText : Palette.secondaryText)
            .padding(.vertical, Metric.rowVPad)
            .padding(.horizontal, Metric.sidebarGutter)
            .frame(maxWidth: .infinity, alignment: .leading)
            .contentShape(RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous))
            .background(
                background,
                in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
            )
        }
        .buttonStyle(.plain)
        .onHover { hovering = $0 }
        .help(live ? "Open \(kind.title)" : "\(kind.title) has no panel yet")
    }

    private var background: Color {
        guard live else { return .clear }
        if selected { return Palette.rowSelected }
        return hovering ? Palette.rowHover : .clear
    }
}

private struct BotRow: View {
    let bot: Bot
    let preview: String?
    let stamp: Date?
    let fallback: String
    let selected: Bool
    let select: () -> Void

    @State private var hovering = false

    var body: some View {
        Button(action: select) {
            HStack(spacing: 8) {
                BotAvatar(botID: bot.id, size: Metric.avatarRow)
                VStack(alignment: .leading, spacing: 1) {
                    HStack(spacing: 6) {
                        Text(bot.displayName)
                            .font(TypeScale.rowTitle)
                            .foregroundStyle(Palette.primaryText)
                            .lineLimit(1)
                        Spacer(minLength: 0)
                        if let stamp {
                            Text(Self.stamped(stamp))
                                .font(TypeScale.rowMeta)
                                .foregroundStyle(Palette.secondaryText)
                        }
                    }
                    HStack(spacing: 5) {
                        Text(preview.map(Self.oneLine) ?? fallback)
                            .font(TypeScale.rowPreview)
                            .foregroundStyle(Palette.secondaryText)
                            .lineLimit(1)
                            .truncationMode(.tail)
                        Spacer(minLength: 0)
                        // A broken model is a property of the bot, not of its
                        // last message, so it gets its own quiet glyph rather
                        // than recoloring the preview text.
                        if bot.isModelBroken {
                            Image(systemName: "exclamationmark.triangle.fill")
                                .font(.system(size: 8))
                                .foregroundStyle(Palette.attention)
                        }
                        if bot.hasUnread {
                            Circle()
                                .fill(Palette.attention)
                                .frame(width: 6, height: 6)
                        }
                    }
                }
            }
            .padding(.vertical, Metric.rowVPad)
            .padding(.horizontal, Metric.sidebarGutter)
            .frame(maxWidth: .infinity, alignment: .leading)
            .contentShape(RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous))
            .background(
                background,
                in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
            )
        }
        .buttonStyle(.plain)
        .onHover { hovering = $0 }
    }

    private var background: Color {
        if selected { return Palette.rowSelected }
        return hovering ? Palette.rowHover : .clear
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
