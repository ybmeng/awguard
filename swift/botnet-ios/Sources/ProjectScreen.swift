// ProjectScreen.swift — one project on the phone: what it is about, how
// healthy that is, and the typed facts the health is derived from. Health,
// nextDue and factCount are computed server-side from the facts, so nothing
// here recomputes them, and the facts render in exactly the urgency-first
// order the server sorted them into.
//
// Read-mostly by decision. The phone can add a fact and tick one done; it
// cannot delete a project, delete a fact, or rename either. Those cascade onto
// facts and their projected calendar events, and a cascade needs a
// confirmation design this version does not have.

import SwiftUI

struct ProjectScreen: View {
    @EnvironmentObject var store: AppStore
    let projectID: String
    /// Opens the Add Fact sheet as the screen appears. Only a launch flag sets
    /// it (see LaunchScreen); nil in every ordinary push.
    var openAddFact = false
    /// Which kind that sheet opens on, when a launch flag opened it.
    var addFactKind: FactKind = .deadline

    @State private var addingFact = false
    /// The flag is honoured once. Without this, dismissing the sheet re-runs
    /// onAppear and the sheet reopens forever.
    @State private var honouredLaunch = false

    /// Resolved on every render, never captured: a bot's project tool writes
    /// the same rows, and a refetch has to reach an open pane.
    private var project: Project? { store.projects.first { $0.id == projectID } }

    var body: some View {
        Group {
            if let project {
                content(project)
            } else {
                VanishedRow(noun: "project")
            }
        }
        .background(Palette.chrome)
        .navigationTitle(project?.name ?? "Project")
        .navigationBarTitleDisplayMode(.inline)
        // Opaque from the first frame, so scrolled facts pass behind the title
        // rather than through it.
        .toolbarBackground(.visible, for: .navigationBar)
        .toolbarBackground(Palette.chrome, for: .navigationBar)
        .toolbar {
            // No project resolved means it is gone from the server; there is
            // nothing to add a fact to.
            if project != nil {
                ToolbarItem(placement: .topBarTrailing) {
                    Button { addingFact = true } label: {
                        Image(systemName: "plus")
                    }
                    .accessibilityLabel("Add fact")
                }
            }
        }
        // A bot's project tool writes the same facts, so the pane re-reads on
        // every open.
        .task(id: projectID) { await store.loadProject(projectID) }
        .onAppear {
            guard openAddFact, !honouredLaunch else { return }
            honouredLaunch = true
            addingFact = true
        }
        .sheet(isPresented: $addingFact) {
            if let project { AddFactSheet(project: project, initialKind: addFactKind) }
        }
    }

    private func content(_ project: Project) -> some View {
        ScrollView {
            VStack(alignment: .leading, spacing: Metric.phoneVPad) {
                if project.hasGoal {
                    Text(project.goal)
                        .font(TypeScale.phoneMessage)
                        .foregroundStyle(Palette.primaryText)
                        .fixedSize(horizontal: false, vertical: true)
                }
                healthBadge(project)
                // Before the facts: a parent's rolled-up reading is often a
                // child's, so the children are what explain the badge above.
                childrenSection
                factsSection
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, Metric.phoneHPad)
            .padding(.vertical, Metric.phoneVPad)
        }
        .refreshable { await store.loadProject(projectID) }
    }

    // The same dot the list row wears, with its reading next to it — a screen
    // has room to say "S0 · overdue 13d" where the row could only color it. On
    // a parent all three are ROLLED UP over the subtree: the severity a parent
    // shows may be a child's, which is why the children strip sits under it.
    private func healthBadge(_ project: Project) -> some View {
        HStack(spacing: Metric.phoneTightGap) {
            Circle()
                .fill(Palette.projectDot(severity: project.severity, health: project.health))
                .frame(width: Metric.healthDot, height: Metric.healthDot)
            Text(project.severityText)
                .font(TypeScale.phoneRowMeta)
                .foregroundStyle(Palette.secondaryText)
        }
        .padding(.horizontal, Metric.phoneRowGap)
        .padding(.vertical, Metric.phoneTightGap)
        .background(Palette.fieldFill, in: Capsule())
    }

    /// The direct children, preferring the live list — a rename or a moved
    /// child reaches an open screen that way — and falling back to the detail's
    /// own `children` for a list this client hasn't refetched yet.
    private var children: [Project] {
        let fromList = ProjectTree(store.projects).children(of: projectID)
        guard fromList.isEmpty else { return fromList }
        return store.projectDetails[projectID]?.children ?? []
    }

    @ViewBuilder
    private var childrenSection: some View {
        if !children.isEmpty {
            Text("Sub-projects")
                .font(TypeScale.phoneDayHeader)
                .foregroundStyle(Palette.secondaryText)
            VStack(alignment: .leading, spacing: Metric.phoneTightGap) {
                ForEach(children) { child in
                    NavigationLink(value: Route.project(child.id)) {
                        HStack(spacing: Metric.phoneRowGap) {
                            Circle()
                                .fill(Palette.projectDot(severity: child.severity,
                                                         health: child.health))
                                .frame(width: Metric.healthDot, height: Metric.healthDot)
                            Text(child.name)
                                .font(TypeScale.phoneRowTitle)
                                .foregroundStyle(Palette.primaryText)
                                .lineLimit(1)
                            Spacer(minLength: 0)
                            Text(child.severityText)
                                .font(TypeScale.phoneRowMeta)
                                .foregroundStyle(Palette.secondaryText)
                            Image(systemName: "chevron.right")
                                .font(TypeScale.phoneRowMeta)
                                .foregroundStyle(Palette.secondaryText)
                        }
                        .frame(minHeight: Metric.phoneTapTarget)
                    }
                    .buttonStyle(.plain)
                }
            }
        }
    }

    @ViewBuilder
    private var factsSection: some View {
        Text("Facts")
            .font(TypeScale.phoneDayHeader)
            .foregroundStyle(Palette.secondaryText)
        // Nil is "still loading", [] is "this project genuinely has none" —
        // the store keeps them apart so the screen can say which.
        if let facts = store.facts(for: projectID) {
            if facts.isEmpty {
                Text("No facts yet. Add a deadline, a recurring obligation, a milestone or a note.")
                    .font(TypeScale.phoneRowPreview)
                    .foregroundStyle(Palette.secondaryText)
                    .fixedSize(horizontal: false, vertical: true)
            } else {
                VStack(alignment: .leading, spacing: Metric.phoneVPad) {
                    ForEach(facts) { fact in
                        FactRow(fact: fact)
                    }
                }
            }
        } else {
            Text("Loading facts…")
                .font(TypeScale.phoneRowPreview)
                .foregroundStyle(Palette.secondaryText)
        }
    }
}

// One fact: kind glyph, done toggle where the kind allows it, title, and the
// dates and blocker that make it urgent.
private struct FactRow: View {
    @EnvironmentObject var store: AppStore
    let fact: ProjectFact

    var body: some View {
        VStack(alignment: .leading, spacing: Metric.phoneTightGap) {
            HStack(spacing: Metric.phoneRowGap) {
                // A completable fact's toggle sits where the glyph would, so
                // the titles still line up on one x; the glyph moves right of
                // it and keeps saying which kind the fact is.
                if fact.isCompletable {
                    Button {
                        Task { await store.updateFact(fact, fields: ["done": !fact.done]) }
                    } label: {
                        Image(systemName: fact.done ? "checkmark.circle.fill" : "circle")
                            .font(.system(size: Metric.phoneFactToggle))
                            .foregroundStyle(fact.done ? Palette.healthOK : Palette.secondaryText)
                            // The glyph is small; the thing a thumb hits is not.
                            .frame(width: Metric.phoneTapTarget,
                                   height: Metric.phoneTapTarget)
                            .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(fact.done ? "Mark not done" : "Mark done")
                } else {
                    // The toggle's full hit width, so a note's title starts at
                    // the same x as a deadline's.
                    Color.clear
                        .frame(width: Metric.phoneTapTarget, height: Metric.phoneFactToggle)
                }
                Image(systemName: FactKind.symbol(for: fact.kind))
                    .font(TypeScale.phoneRowMeta)
                    .foregroundStyle(Palette.secondaryText)
                    .frame(width: Metric.factGlyphWidth)
                Text(fact.title)
                    .font(TypeScale.phoneRowTitle)
                    .foregroundStyle(fact.done ? Palette.secondaryText : Palette.primaryText)
                    .strikethrough(fact.done)
                    .fixedSize(horizontal: false, vertical: true)
                Spacer(minLength: 0)
            }
            // Everything below the title shares one indent. The Mac keeps the
            // due column beside the title in a 680pt list; 393pt cannot hold a
            // date, a lead window and a relative reading next to a name, and a
            // truncated due date is worse than a moved one.
            VStack(alignment: .leading, spacing: Metric.phoneTightGap) {
                if fact.isBlocked, !fact.done { blockerChip }
                dueLine
                // An RRULE is long, and squeezed into a row it truncates
                // mid-rule — half a recurrence rule is worse than none, since
                // the rule IS the spec.
                if let rrule = fact.rrule, !rrule.isEmpty {
                    Text(rrule)
                        .font(TypeScale.phoneCodeBlock)
                        .foregroundStyle(Palette.secondaryText)
                        .textSelection(.enabled)
                }
                if let body = fact.body, !body.isEmpty {
                    Text(body)
                        .font(TypeScale.phoneRowPreview)
                        .foregroundStyle(Palette.secondaryText)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            .padding(.leading, Metric.phoneTapTarget + Metric.factGlyphWidth
                     + 2 * Metric.phoneRowGap)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    // Named, not just colored: "blocked" on a milestone means a human has to
    // move, which is the one thing a glance at this screen must surface.
    private var blockerChip: some View {
        Text(fact.blocker ?? "")
            .font(TypeScale.phoneRowMeta)
            .foregroundStyle(Palette.healthBlocked)
            .padding(.horizontal, Metric.phoneRowGap)
            .padding(.vertical, Metric.phoneTightGap)
            .background(
                Palette.healthBlocked.opacity(0.12),
                in: RoundedRectangle(cornerRadius: Metric.chipRadius, style: .continuous)
            )
    }

    // The dates. A DONE fact shows its date and nothing else: it no longer
    // counts toward health, so "overdue 49d" on a finished deadline was an
    // alarm about something already handled, and its lead window — the window
    // that would have made it due_soon — is equally spent.
    @ViewBuilder
    private var dueLine: some View {
        if let due = fact.due {
            HStack(spacing: Metric.phoneRowGap) {
                if !fact.done, fact.leadDays > 0 {
                    Text("\(fact.leadDays)d lead")
                        .font(TypeScale.phoneRowMeta)
                        .foregroundStyle(Palette.secondaryText)
                }
                Text(due.formatted(.dateTime.year().month(.abbreviated).day()))
                    .font(TypeScale.phoneRowMeta)
                    .foregroundStyle(Palette.secondaryText)
                if !fact.done, let relative = fact.dueText {
                    Text(relative)
                        .font(TypeScale.phoneRowMeta.weight(.semibold))
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

/// New project: a name and an optional goal, nothing else. Health, nextDue and
/// factCount are derived server-side from facts that do not exist yet, so there
/// is deliberately nothing to set for them here.
struct NewProjectSheet: View {
    @EnvironmentObject var store: AppStore
    @Environment(\.dismiss) private var dismiss

    @State private var name = ""
    @State private var goal = ""
    @State private var parentID = ""
    @State private var saving = false

    private var trimmedName: String { name.trimmingCharacters(in: .whitespacesAndNewlines) }

    /// Every project is a candidate: a project being created is nobody's
    /// ancestor yet, so no cycle is possible and nothing has to be excluded.
    private var candidates: [ProjectTree.Row] {
        let tree = ProjectTree(store.projects)
        return tree.rows(expanded: Set(tree.all.map(\.id)))
    }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("Name", text: $name)
                    Picker("Parent", selection: $parentID) {
                        Text("None (top level)").tag("")
                        ForEach(candidates) { row in
                            // The depth is spelled in the label because a Picker
                            // menu has no indent of its own.
                            Text(String(repeating: "· ", count: row.depth) + row.project.name)
                                .tag(row.project.id)
                        }
                    }
                }
                Section("Goal") {
                    TextField("What this project is for", text: $goal, axis: .vertical)
                        .lineLimit(3...8)
                }
            }
            .font(TypeScale.phoneMessage)
            .navigationTitle("New Project")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button(saving ? "Saving…" : "Create") {
                        saving = true
                        Task {
                            let created = await store.createProject(
                                name: trimmedName, goal: cleaned(goal), parentID: parentID)
                            saving = false
                            // A failed create (duplicate name) keeps the sheet
                            // open with the draft; the shared alert explains.
                            if created != nil { dismiss() }
                        }
                    }
                    .disabled(saving || trimmedName.isEmpty)
                }
            }
        }
    }

    private func cleaned(_ text: String) -> String {
        text.trimmingCharacters(in: .whitespacesAndNewlines)
    }
}

/// Add one typed fact. The kind picker is the whole form: `FactKind.fields` is
/// the single table saying which inputs a kind takes, and the rows below read
/// that set. Adding a kind is a case plus a row in that table — never another
/// branch here — and an input the server would reject for this kind is never
/// shown, so the contract's 400s stay a backstop rather than a routine answer.
struct AddFactSheet: View {
    @EnvironmentObject var store: AppStore
    @Environment(\.dismiss) private var dismiss

    let project: Project
    /// Which kind the sheet opens on. Deadline in the app, which is the kind a
    /// project usually starts with; a launch flag names another so the field
    /// table's effect on the form can be seen on a simulator, where the picker
    /// cannot be tapped.
    var initialKind: FactKind = .deadline

    @State private var kind: FactKind
    @State private var title = ""
    @State private var due = AddFactSheet.defaultDue()
    @State private var leadDays = 30
    @State private var rrule = ""
    @State private var tz = TimeZone.current.identifier
    @State private var blocker = ""
    @State private var body_ = ""
    @State private var saving = false

    init(project: Project, initialKind: FactKind = .deadline) {
        self.project = project
        self.initialKind = initialKind
        _kind = State(initialValue: initialKind)
    }

    private var trimmedTitle: String { title.trimmingCharacters(in: .whitespacesAndNewlines) }

    /// Recurring REQUIRES an rrule and a tz server-side, so the sheet refuses
    /// to send one without them rather than surfacing the 400.
    private var canSave: Bool {
        guard !saving, !trimmedTitle.isEmpty else { return false }
        guard kind.fields.contains(.rrule) else { return true }
        return !cleaned(rrule).isEmpty && !tz.isEmpty
    }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Picker("Kind", selection: $kind) {
                        ForEach(FactKind.allCases) { k in
                            Text(k.title).tag(k)
                        }
                    }
                    TextField("Title", text: $title)
                    if kind.fields.contains(.due) {
                        DatePicker(kind == .recurring ? "First due" : "Due",
                                   selection: $due,
                                   displayedComponents: [.date, .hourAndMinute])
                    }
                    if kind.fields.contains(.leadDays) {
                        // The window that makes this fact "due soon". The
                        // server's own default is 30 days.
                        Stepper("Lead \(leadDays) days", value: $leadDays, in: 0...365)
                    }
                    if kind.fields.contains(.rrule) {
                        // Typed verbatim: the rule IS the spec, and the server
                        // parses its own subset. Paraphrasing it in a picker
                        // would hide what actually recurs.
                        stacked("Repeat rule") {
                            TextField("Repeat rule", text: $rrule,
                                      prompt: Text("FREQ=YEARLY;BYMONTH=11;BYMONTHDAY=30"))
                                .textInputAutocapitalization(.never)
                                .autocorrectionDisabled()
                        }
                    }
                    if kind.fields.contains(.tz) {
                        stacked("Time zone") {
                            TextField("Time zone", text: $tz)
                                .textInputAutocapitalization(.never)
                                .autocorrectionDisabled()
                        }
                    }
                    if kind.fields.contains(.blocker) {
                        stacked("Blocker") {
                            TextField("Blocker", text: $blocker,
                                      prompt: Text("who or what this waits on"))
                        }
                    }
                }
                if kind.fields.contains(.body) {
                    Section(kind == .note ? "Note" : "Details") {
                        TextField("",
                                  text: $body_,
                                  prompt: Text(kind == .note ? "what to remember"
                                               : "anything worth knowing about this"),
                                  axis: .vertical)
                            .lineLimit(3...8)
                    }
                }
            }
            .font(TypeScale.phoneMessage)
            .navigationTitle("Add Fact")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button(saving ? "Saving…" : "Add", action: save)
                        .disabled(!canSave)
                }
            }
        }
    }

    /// A field whose label sits ABOVE it. iOS drops a TextField's label the
    /// moment it carries a prompt, which left a raw RRULE and a bare
    /// "Asia/Shanghai" sitting in the form with nothing saying what they were.
    /// Stacked rather than leading, because a rule is far too long to share a
    /// row with its name.
    private func stacked<Field: View>(_ label: String,
                                      @ViewBuilder field: () -> Field) -> some View {
        VStack(alignment: .leading, spacing: Metric.phoneTightGap) {
            Text(label)
                .font(TypeScale.phoneRowMeta)
                .foregroundStyle(Palette.secondaryText)
            field().labelsHidden()
        }
    }

    // A failed save keeps the sheet open with the draft intact; the error is
    // already on its way to the one shared alert.
    private func save() {
        saving = true
        Task {
            let saved = await store.addFact(to: project.id, fields: wireFields())
            saving = false
            if saved { dismiss() }
        }
    }

    /// The create body, built from the SAME field table the form reads — so a
    /// key the picked kind doesn't allow can't reach the wire even if a stale
    /// draft still holds a value for it.
    private func wireFields() -> [String: Any] {
        var fields: [String: Any] = ["kind": kind.rawValue, "title": trimmedTitle]
        if kind.fields.contains(.due) { fields["due"] = APIClient.wireTime(due) }
        if kind.fields.contains(.leadDays) { fields["leadDays"] = leadDays }
        if kind.fields.contains(.rrule), !cleaned(rrule).isEmpty {
            fields["rrule"] = cleaned(rrule)
        }
        if kind.fields.contains(.tz), !cleaned(tz).isEmpty { fields["tz"] = cleaned(tz) }
        if kind.fields.contains(.blocker), !cleaned(blocker).isEmpty {
            fields["blocker"] = cleaned(blocker)
        }
        if kind.fields.contains(.body), !cleaned(body_).isEmpty {
            fields["body"] = cleaned(body_)
        }
        return fields
    }

    private func cleaned(_ text: String) -> String {
        text.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// A new deadline opens a month out at 9am: a due date typed today is
    /// almost never today, and midnight would read as the day before in half
    /// the world's timezones.
    private static func defaultDue(now: Date = Date()) -> Date {
        let calendar = Calendar.current
        let month = calendar.date(byAdding: .month, value: 1, to: now) ?? now
        return calendar.date(bySettingHour: 9, minute: 0, second: 0, of: month) ?? month
    }
}
