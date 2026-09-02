// ProjectView.swift — one project's pane: where it sits in the tree, what it
// is about, how urgent that is, and the typed facts and sub-projects the
// urgency is derived from. The server owns all of it; health, severity and
// nextDue are computed there and ROLLED UP over the subtree, so nothing in this
// file recomputes them — the pane reads what the server already decided, and
// the facts render in exactly the urgency-first order it sorted them into.

import SwiftUI

struct ProjectView: View {
    @EnvironmentObject var store: AppStore
    let project: Project
    /// Selecting a sub-project from the children strip. The pane does not own
    /// the sidebar selection, so it hands the id up rather than keeping a
    /// second idea of what is selected. No-op in the snapshot tool.
    var select: (String) -> Void = { _ in }

    @State private var sheet: ProjectSheet?
    @State private var confirmingDelete = false

    /// The SAME tree the sidebar draws, so the pane's parent link, its children
    /// strip and its delete warning can never disagree with the sidebar: the
    /// flat list is re-read after every write and after every bot turn, while a
    /// cached detail is only re-read for the project it belongs to.
    private var tree: ProjectTree { ProjectTree(store.projects) }

    private var parent: Project? { tree.project(project.parentId ?? "") }

    /// The detail's own `children` is the fallback for a list that hasn't
    /// arrived yet — the route answers for this project specifically.
    private var children: [Project] {
        let fromList = tree.children(of: project.id)
        guard fromList.isEmpty else { return fromList }
        return store.projectDetails[project.id]?.children ?? []
    }

    var body: some View {
        VStack(spacing: 0) {
            header
            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    if project.hasGoal {
                        Text(project.goal)
                            .font(TypeScale.message)
                            .foregroundStyle(Palette.primaryText)
                    }
                    // Above the facts: a parent's own facts are usually the
                    // smaller half of what it is about, and the sub-projects
                    // are what its rolled-up severity is actually reading.
                    if !children.isEmpty { childrenSection }
                    factsSection
                }
                .frame(maxWidth: Metric.projectListWidth, alignment: .leading)
                .padding(.horizontal, Metric.transcriptHPad)
                .padding(.vertical, Metric.transcriptVPad)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .background(Palette.chrome)
        // A bot's project tool writes the same facts, so the pane re-reads on
        // every open and on every switch to a different project.
        .task(id: project.id) { await store.loadProject(project.id) }
        .onChange(of: project.id) { sheet = nil }
        .sheet(item: $sheet) { which in
            switch which {
            case .addFact: AddFactSheet(project: project)
            case .edit: EditProjectSheet(project: project)
            case .newSubProject:
                NewProjectSheet(parent: project) { created in select(created.id) }
            }
        }
        .alert("Delete project?", isPresented: $confirmingDelete) {
            Button("Delete \"\(project.name)\"", role: .destructive) {
                Task { await store.deleteProject(project) }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text(deleteWarning)
        }
    }

    // The cascade is the whole reason the alert exists, so what goes with the
    // project is in the sentence rather than implied — and with a hierarchy the
    // sub-projects go too, which is the part a user cannot see from here.
    private var deleteWarning: String {
        let descendants = tree.subtree(of: project.id).dropFirst()
        var takes = [factCountPhrase]
        if !descendants.isEmpty {
            takes.insert(descendants.count == 1
                         ? "1 sub-project" : "\(descendants.count) sub-projects", at: 0)
            takes.append("everything under them")
        }
        return "Deleting \"\(project.name)\" also deletes \(list(takes)), including any calendar events projected from them. This can't be undone."
    }

    private var factCountPhrase: String {
        let count = store.facts(for: project.id)?.count ?? project.factCount
        return count == 1 ? "its 1 fact" : "its \(count) facts"
    }

    private func list(_ parts: [String]) -> String {
        guard parts.count > 1 else { return parts.first ?? "" }
        return parts.dropLast().joined(separator: ", ") + " and " + parts[parts.count - 1]
    }

    private var header: some View {
        HStack(spacing: 8) {
            // Where this sits in the tree. Without it a sub-project's pane is
            // indistinguishable from a top-level one, and the Edit sheet's
            // picker would be the only place that says which parent it is
            // under. Clickable, like the children strip, so the hierarchy
            // walks both ways.
            if let parent {
                Button { select(parent.id) } label: {
                    HStack(spacing: 4) {
                        Text(parent.name).lineLimit(1)
                        Image(systemName: "chevron.right")
                            .font(TypeScale.sectionChevron)
                    }
                    .font(TypeScale.headerTitle)
                    .foregroundStyle(Palette.secondaryText)
                }
                .buttonStyle(.plain)
                .help("Open \(parent.name)")
                .layoutPriority(-1)
            }
            Text(project.name)
                .font(TypeScale.headerTitle)
                .foregroundStyle(Palette.primaryText)
                .lineLimit(1)
            healthBadge
            Spacer()
            Button("New Sub-project") { sheet = .newSubProject }
                .help("Create a project under \(project.name)")
            Button("Add Fact") { sheet = .addFact }
                .help("Add a deadline, recurring obligation, milestone or note")
            Button { sheet = .edit } label: {
                Image(systemName: "pencil").foregroundStyle(Palette.secondaryText)
            }
            .buttonStyle(.borderless)
            .help("Edit name, goal and parent")
            Button { confirmingDelete = true } label: {
                Image(systemName: "trash").foregroundStyle(Palette.secondaryText)
            }
            .buttonStyle(.borderless)
            .help("Delete \(project.name), its facts and everything under it")
        }
        .padding(.horizontal, Metric.transcriptHPad)
        .frame(height: Metric.headerHeight)
        .overlay(alignment: .bottom) {
            Rectangle().fill(Palette.hairline).frame(height: 1)
        }
    }

    // The same dot the sidebar row wears, with its words next to it — the pane
    // has room to say "S0 · overdue 13d" where the row could only color it.
    // Built like the automation pane's freshness badge for that reason.
    //
    // On a parent, all three readings are ROLLED UP: the severity, the health
    // word and the relative due come from the whole subtree, so a parent with
    // no facts of its own can still read S0.
    private var healthBadge: some View {
        HStack(spacing: 5) {
            Circle()
                .fill(Palette.projectDot(severity: project.severity, health: project.health))
                .frame(width: Metric.healthDot, height: Metric.healthDot)
            Text(project.severityText)
                .font(TypeScale.rowMeta)
                .foregroundStyle(Palette.secondaryText)
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 3)
        .background(Palette.fieldFill, in: Capsule())
    }

    // The sub-projects, as links rather than as a second tree: the sidebar is
    // where the hierarchy is navigated, and duplicating its disclosure here
    // would be a second walk to keep in step. Each row says what the parent's
    // rolled-up badge is reading from.
    @ViewBuilder
    private var childrenSection: some View {
        Text("Sub-projects")
            .font(TypeScale.sectionLabel)
            .foregroundStyle(Palette.secondaryText)
        VStack(alignment: .leading, spacing: Metric.childStripRowGap) {
            ForEach(children) { child in
                ChildRow(child: child) { select(child.id) }
            }
        }
    }

    @ViewBuilder
    private var factsSection: some View {
        Text("Facts")
            .font(TypeScale.sectionLabel)
            .foregroundStyle(Palette.secondaryText)
        // Nil is "still loading", [] is "this project genuinely has none" —
        // the store keeps them apart so the pane can say which.
        if let facts = store.facts(for: project.id) {
            if facts.isEmpty {
                Text("No facts yet. Add a deadline, a recurring obligation, a milestone or a note.")
                    .font(TypeScale.rowPreview)
                    .foregroundStyle(Palette.secondaryText)
            } else {
                VStack(alignment: .leading, spacing: Metric.eventRowGap) {
                    ForEach(facts) { fact in
                        FactRow(fact: fact)
                    }
                }
            }
        } else {
            Text("Loading facts…")
                .font(TypeScale.rowPreview)
                .foregroundStyle(Palette.secondaryText)
        }
    }
}

/// Which sheet the pane has up. Identifiable so `.sheet(item:)` builds a fresh
/// form per target rather than reusing the other one's draft.
enum ProjectSheet: String, Identifiable {
    case addFact, edit, newSubProject

    var id: String { rawValue }
}

// One sub-project in the pane's strip: severity dot, name, relative due. The
// same three things the sidebar row shows, because it IS that row's content —
// clicking it selects the child, so the two must read as the same object.
private struct ChildRow: View {
    let child: Project
    let select: () -> Void

    @State private var hovering = false

    var body: some View {
        Button(action: select) {
            HStack(spacing: 8) {
                // The fact rows' toggle slot, empty here, so the two lists
                // stacked in this pane share one text column — and the dot
                // lands exactly where a fact's kind glyph does.
                Color.clear.frame(width: Metric.factToggle, height: Metric.factToggle)
                Circle()
                    .fill(Palette.projectDot(severity: child.severity, health: child.health))
                    .frame(width: Metric.healthDot, height: Metric.healthDot)
                    .frame(width: Metric.factGlyphWidth)
                Text(child.name)
                    .font(TypeScale.rowTitle)
                    .foregroundStyle(Palette.primaryText)
                    .lineLimit(1)
                if child.hasChildren {
                    // A grandchild is real work this row stands for but does
                    // not list; the sidebar is where it opens.
                    Text(child.childCount == 1
                         ? "1 sub-project" : "\(child.childCount) sub-projects")
                        .font(TypeScale.rowMeta)
                        .foregroundStyle(Palette.secondaryText)
                }
                Spacer(minLength: 0)
                if let due = child.nextDueText {
                    Text(due)
                        .font(TypeScale.rowMeta.weight(.semibold))
                        .foregroundStyle(child.health == "overdue"
                                         ? Palette.healthOverdue : Palette.secondaryText)
                }
            }
            .padding(.vertical, Metric.rowVPad)
            .padding(.horizontal, Metric.sidebarGutter)
            .frame(maxWidth: .infinity, alignment: .leading)
            .contentShape(RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous))
            .background(
                hovering ? Palette.rowHover : .clear,
                in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
            )
        }
        .buttonStyle(.plain)
        .onHover { hovering = $0 }
        .help("\(child.name) — \(child.severityText)")
    }
}

// One fact: kind glyph, done toggle where the kind allows it, title, and the
// dates and blocker that make it urgent. Its own struct so the delete
// confirmation's state is per-row.
private struct FactRow: View {
    @EnvironmentObject var store: AppStore
    let fact: ProjectFact

    @State private var hovering = false
    @State private var confirmingDelete = false

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 8) {
                // A completable fact's toggle sits where the glyph would, so
                // the titles still line up on one x; the glyph moves right of
                // it and keeps saying which kind the fact is.
                if fact.isCompletable {
                    Button {
                        Task { await store.updateFact(fact, fields: ["done": !fact.done]) }
                    } label: {
                        Image(systemName: fact.done ? "checkmark.circle.fill" : "circle")
                            .font(.system(size: Metric.factToggle))
                            .foregroundStyle(fact.done ? Palette.healthOK : Palette.secondaryText)
                    }
                    .buttonStyle(.plain)
                    .help(fact.done ? "Mark not done" : "Mark done")
                } else {
                    Color.clear.frame(width: Metric.factToggle, height: Metric.factToggle)
                }
                Image(systemName: FactKind.symbol(for: fact.kind))
                    .font(TypeScale.eventGlyph)
                    .foregroundStyle(Palette.secondaryText)
                    .frame(width: Metric.factGlyphWidth)
                    .help(fact.kind)
                Text(fact.title)
                    .font(TypeScale.rowTitle)
                    .foregroundStyle(fact.done ? Palette.secondaryText : Palette.primaryText)
                    .strikethrough(fact.done)
                    .lineLimit(1)
                if fact.isBlocked, !fact.done {
                    blockerChip
                }
                Spacer(minLength: 0)
                dueColumn
                Button { confirmingDelete = true } label: {
                    Image(systemName: "trash")
                        .font(TypeScale.eventGlyph)
                        .foregroundStyle(Palette.secondaryText)
                }
                .buttonStyle(.borderless)
                // Only on hover: a trash can beside every fact would make the
                // list read as a list of things to remove.
                .opacity(hovering ? 1 : 0)
                .help("Delete this fact")
            }
            // The rule and the body sit under the title rather than in the
            // trailing cluster: an RRULE is long, and squeezed into the row it
            // truncated mid-rule — a half-printed recurrence rule is worse than
            // none, since the rule IS the spec.
            if fact.isRecurring || fact.hasBody {
                VStack(alignment: .leading, spacing: 2) {
                    if let rrule = fact.rrule, !rrule.isEmpty {
                        Text(rrule)
                            .font(TypeScale.codeBlock)
                            .foregroundStyle(Palette.secondaryText)
                            .textSelection(.enabled)
                    }
                    if let body = fact.body, !body.isEmpty {
                        Text(body)
                            .font(TypeScale.rowPreview)
                            .foregroundStyle(Palette.secondaryText)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .padding(.leading, Metric.factToggle + Metric.factGlyphWidth + 16)
            }
        }
        .padding(.vertical, Metric.rowVPad)
        .padding(.horizontal, Metric.sidebarGutter)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            hovering ? Palette.rowHover : .clear,
            in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
        )
        .onHover { hovering = $0 }
        .alert("Delete fact?", isPresented: $confirmingDelete) {
            Button("Delete \"\(fact.title)\"", role: .destructive) {
                Task { await store.deleteFact(fact) }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("This also deletes the calendar event projected from it, if there is one.")
        }
    }

    // Named, not just colored: "blocked" on a milestone means a human has to
    // move, which is the one thing a glance at this pane must surface.
    private var blockerChip: some View {
        Text(fact.blocker ?? "")
            .font(TypeScale.rowMeta)
            .foregroundStyle(Palette.healthBlocked)
            .lineLimit(1)
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(
                Palette.healthBlocked.opacity(0.12),
                in: RoundedRectangle(cornerRadius: Metric.chipRadius, style: .continuous)
            )
    }

    // The date column. A DONE fact shows its date and nothing else: it no
    // longer counts toward health, so "overdue 49d" on a finished deadline was
    // an alarm about something already handled, and its lead window — the
    // window that would have made it due_soon — is equally spent.
    @ViewBuilder
    private var dueColumn: some View {
        if let due = fact.due {
            HStack(spacing: 6) {
                if !fact.done {
                    if fact.leadDays > 0 {
                        Text("\(fact.leadDays)d lead")
                            .font(TypeScale.rowMeta)
                            .foregroundStyle(Palette.secondaryText)
                    }
                }
                Text(due.formatted(.dateTime.year().month(.abbreviated).day()))
                    .font(TypeScale.rowMeta)
                    .foregroundStyle(Palette.secondaryText)
                if !fact.done, let relative = fact.dueText {
                    Text(relative)
                        .font(TypeScale.rowMeta.weight(.semibold))
                        .foregroundStyle(relativeColor)
                }
            }
        }
    }

    // Only reached for an undone fact now, but it stays defensive: the done
    // check is the semantic rule, not an accident of the caller.
    private var relativeColor: Color {
        guard !fact.done, let due = fact.due else { return Palette.secondaryText }
        if due < Date() { return Palette.healthOverdue }
        // The fact's own lead window is what the server calls due_soon, so the
        // row colors on exactly the same boundary rather than a second rule.
        if fact.leadDays > 0,
           due < Date().addingTimeInterval(Double(fact.leadDays) * 86_400) {
            return Palette.healthDueSoon
        }
        return Palette.secondaryText
    }
}
