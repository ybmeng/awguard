// ProjectView.swift — one project's pane: what it is about, how healthy that
// is, and the typed facts the health is derived from. The server owns all of
// it; health and nextDue are computed there from the facts, so nothing in this
// file recomputes them — the pane reads what the server already decided, and
// the facts render in exactly the urgency-first order it sorted them into.

import SwiftUI

struct ProjectView: View {
    @EnvironmentObject var store: AppStore
    let project: Project

    @State private var sheet: ProjectSheet?
    @State private var confirmingDelete = false

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
            }
        }
        .alert("Delete project?", isPresented: $confirmingDelete) {
            Button("Delete \"\(project.name)\"", role: .destructive) {
                Task { await store.deleteProject(project) }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Deleting \"\(project.name)\" also deletes \(factCountPhrase) and any calendar events projected from them. This can't be undone.")
        }
    }

    // The cascade is the whole reason the alert exists, so the count is in the
    // sentence rather than implied — same stance as the calendar's delete.
    private var factCountPhrase: String {
        let count = store.facts(for: project.id)?.count ?? project.factCount
        return count == 1 ? "its 1 fact" : "its \(count) facts"
    }

    private var header: some View {
        HStack(spacing: 8) {
            Text(project.name)
                .font(TypeScale.headerTitle)
                .foregroundStyle(Palette.primaryText)
                .lineLimit(1)
            healthBadge
            Spacer()
            Button("Add Fact") { sheet = .addFact }
                .help("Add a deadline, recurring obligation, milestone or note")
            Button { sheet = .edit } label: {
                Image(systemName: "pencil").foregroundStyle(Palette.secondaryText)
            }
            .buttonStyle(.borderless)
            .help("Edit name and goal")
            Button { confirmingDelete = true } label: {
                Image(systemName: "trash").foregroundStyle(Palette.secondaryText)
            }
            .buttonStyle(.borderless)
            .help("Delete \(project.name) and its facts")
        }
        .padding(.horizontal, Metric.transcriptHPad)
        .frame(height: Metric.headerHeight)
        .overlay(alignment: .bottom) {
            Rectangle().fill(Palette.hairline).frame(height: 1)
        }
    }

    // The same dot the sidebar row wears, with its word next to it — the pane
    // has room to say "due soon" where the row could only color it. Built like
    // the automation pane's freshness badge for that reason.
    private var healthBadge: some View {
        HStack(spacing: 5) {
            Circle()
                .fill(Palette.health(project.health))
                .frame(width: Metric.healthDot, height: Metric.healthDot)
            Text(badgeText)
                .font(TypeScale.rowMeta)
                .foregroundStyle(Palette.secondaryText)
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 3)
        .background(Palette.fieldFill, in: Capsule())
    }

    /// "overdue 13d" / "due soon · in 18d" / "ok · in 193d" / "unknown". When
    /// the relative reading already carries the health word it stands alone:
    /// "overdue · overdue 13d" says the same thing twice.
    private var badgeText: String {
        guard let due = project.nextDueText else { return project.healthLabel }
        guard !due.hasPrefix(project.healthLabel) else { return due }
        return "\(project.healthLabel) · \(due)"
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
    case addFact, edit

    var id: String { rawValue }
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
