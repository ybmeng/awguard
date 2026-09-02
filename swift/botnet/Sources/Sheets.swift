// Sheets.swift — the create-bot, settings and event sheets. All of them write
// to botnetd; the app stores nothing (the OpenRouter key lives on the server).

import SwiftUI

struct NewBotSheet: View {
    @EnvironmentObject var store: AppStore
    @Environment(\.dismiss) private var dismiss

    @State private var name = ""
    @State private var systemPrompt = ""
    @State private var model = ModelOption.roster[0].id

    var body: some View {
        NavigationStack {
            Form {
                Section("Name") {
                    TextField("e.g. Ada", text: $name)
                }
                Section("Model") {
                    Picker("Model", selection: $model) {
                        ForEach(store.models) { opt in
                            Text(opt.name).tag(opt.id)
                        }
                    }
                    .labelsHidden()
                }
                Section("System prompt") {
                    TextField("You are…", text: $systemPrompt, axis: .vertical)
                        .lineLimit(3...10)
                }
            }
            .formStyle(.grouped)
            .frame(minWidth: 380, minHeight: 340)
            .navigationTitle("New Bot")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Create") {
                        let n = name, sp = systemPrompt, m = model
                        Task { await store.createBot(displayName: n, systemPrompt: sp, model: m) }
                        dismiss()
                    }
                    .disabled(name.trimmingCharacters(in: .whitespaces).isEmpty)
                }
            }
        }
    }
}

/// What the event sheet is editing. Identifiable so `.sheet(item:)` builds a
/// fresh form per target instead of reusing another event's draft.
enum EventTarget: Identifiable {
    case new
    /// A blank event on a day the user picked in the month grid.
    case newOn(Date)
    case existing(Event)

    var id: String {
        switch self {
        case .new: return "new"
        case .newOn(let day): return "new-\(day.timeIntervalSinceReferenceDate)"
        case .existing(let event): return event.id
        }
    }

    var event: Event? {
        guard case .existing(let event) = self else { return nil }
        return event
    }

    var day: Date? {
        guard case .newOn(let day) = self else { return nil }
        return day
    }
}

/// One form for both creating and editing, because the fields are the same and
/// a second near-identical sheet would only drift. Saves are explicit: a bot's
/// calendar tool can write the same event, and last-write-wins is the accepted
/// semantics, so nothing here autosaves.
struct EventSheet: View {
    @EnvironmentObject var store: AppStore
    @Environment(\.dismiss) private var dismiss

    let target: EventTarget

    @State private var title: String
    @State private var starts: Date
    @State private var ends: Date
    @State private var location: String
    @State private var notes: String
    @State private var calendarId: String?
    @State private var saving = false

    /// What the calendar selection opened as. Nil means no calendar could be
    /// defaulted (no filter, no "Personal") — the picker then carries a nil
    /// "Personal" row, honest because an omitted calendarId is exactly what
    /// the server files under Personal, creating it if needed.
    private let initialCalendarId: String?

    /// `defaultCalendarId` is where a *new* event files: CalendarView passes
    /// the active filter's calendar, else its "Personal", else nil. An existing
    /// event's own calendar always wins over it.
    init(target: EventTarget, defaultCalendarId: String? = nil) {
        self.target = target
        let event = target.event
        let start = event?.startsAt ?? Self.defaultStart(on: target.day)
        _title = State(initialValue: event?.title ?? "")
        _starts = State(initialValue: start)
        _ends = State(initialValue: event?.endsAt ?? start.addingTimeInterval(Self.defaultDuration))
        _location = State(initialValue: event?.location ?? "")
        _notes = State(initialValue: event?.notes ?? "")
        initialCalendarId = event.map { $0.calendarId } ?? defaultCalendarId
        _calendarId = State(initialValue: initialCalendarId)
    }

    private var trimmedTitle: String { title.trimmingCharacters(in: .whitespacesAndNewlines) }
    private var canSave: Bool { !saving && !trimmedTitle.isEmpty && ends >= starts }

    var body: some View {
        NavigationStack {
            // Labelled rows rather than a section per field: a macOS Form puts
            // a TextField's placeholder in the label slot, so a section header
            // above it would print the field's name twice on an event that
            // already has a value.
            Form {
                Section {
                    TextField("Title", text: $title)
                    DatePicker("Starts", selection: $starts,
                               displayedComponents: [.date, .hourAndMinute])
                    DatePicker("Ends", selection: $ends, in: starts...,
                               displayedComponents: [.date, .hourAndMinute])
                    TextField("Location", text: $location)
                    // Hidden while there are no calendars to pick between —
                    // both on an old server and on a new one before the first
                    // calendar exists; the unqualified save is legal either way.
                    if !store.calendars.isEmpty {
                        Picker("Calendar", selection: $calendarId) {
                            // A nil row only when the sheet opened without a
                            // calendar: picking a real one must not offer a way
                            // back to "unspecified" that a PATCH can't express.
                            if initialCalendarId == nil {
                                Text("Personal").tag(String?.none)
                            }
                            ForEach(store.calendars) { calendar in
                                Text(calendar.name).tag(String?.some(calendar.id))
                            }
                        }
                    }
                }
                // Read-only by decision: bots and the automations service
                // author recurrence; the sheet states it rather than editing
                // it, verbatim (the rrule IS the spec, paraphrasing would
                // hide what actually fires). Editing recurrence here is OPEN
                // in the contract, deferred.
                if let event = target.event, event.isRecurring || event.firesAutomation {
                    Section {
                        if event.isRecurring {
                            LabeledContent("Repeats") {
                                Text(Self.recurrenceSummary(event))
                                    .foregroundStyle(Palette.secondaryText)
                                    .multilineTextAlignment(.trailing)
                            }
                        }
                        if event.firesAutomation {
                            LabeledContent("Fires") {
                                Label(event.automation ?? "", systemImage: "bolt.fill")
                                    .foregroundStyle(Palette.attention)
                            }
                        }
                    }
                }
                Section("Notes") {
                    TextField("", text: $notes, axis: .vertical)
                        .lineLimit(3...8)
                }
            }
            .formStyle(.grouped)
            .frame(minWidth: 420, minHeight: 380)
            .navigationTitle(target.event == nil ? "New Event" : "Event")
            // Dragging the start past the end would otherwise leave a range the
            // server rejects; carry the duration along instead of refusing it.
            .onChange(of: starts) {
                if ends < starts { ends = starts.addingTimeInterval(Self.defaultDuration) }
            }
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button(saving ? "Saving…" : "Save", action: save)
                        .disabled(!canSave)
                }
            }
        }
    }

    // A failed save keeps the sheet open with the draft intact; the error is
    // already on its way to the one shared alert.
    private func save() {
        saving = true
        Task {
            let saved: Bool
            switch target {
            // Both blank cases create the same way: the picked day only chose
            // where the form opened, it is not a different kind of write.
            case .new, .newOn:
                saved = await store.createEvent(
                    title: trimmedTitle, startsAt: starts, endsAt: ends,
                    location: cleaned(location), notes: cleaned(notes),
                    calendarId: calendarId)
            case .existing(let event):
                saved = await store.updateEvent(event, fields: changes(from: event))
            }
            saving = false
            if saved { dismiss() }
        }
    }

    // Only what actually changed goes on the wire. Events are not If-Match
    // versioned, so resending an untouched field would quietly clobber whatever
    // a bot wrote to it while this sheet was open.
    private func changes(from event: Event) -> [String: String] {
        var fields: [String: String] = [:]
        if trimmedTitle != event.title { fields["title"] = trimmedTitle }
        if starts != event.startsAt { fields["startsAt"] = APIClient.wireTime(starts) }
        if ends != event.endsAt { fields["endsAt"] = APIClient.wireTime(ends) }
        if cleaned(location) != event.location ?? "" { fields["location"] = cleaned(location) }
        if cleaned(notes) != event.notes ?? "" { fields["notes"] = cleaned(notes) }
        // Only a real move goes on the wire; a nil selection means the picker
        // never left the calendar the event was already in.
        if let calendarId, calendarId != event.calendarId { fields["calendarId"] = calendarId }
        return fields
    }

    private func cleaned(_ text: String) -> String {
        text.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// "FREQ=MONTHLY;BYDAY=4TU · America/New_York" — the rule verbatim with
    /// its timezone, the same separator the day headers use. tz is required
    /// with rrule server-side, but a missing one still reads cleanly here.
    private static func recurrenceSummary(_ event: Event) -> String {
        guard let tz = event.tz, !tz.isEmpty else { return event.rrule ?? "" }
        return "\(event.rrule ?? "") · \(tz)"
    }

    private static let defaultDuration: TimeInterval = 3600

    /// Where a blank event starts. A day picked in the month grid opens at 9am
    /// on that day — a day's first event rarely starts at midnight — while
    /// today, and the plain + button, keep the next-full-hour behaviour.
    private static func defaultStart(on day: Date?, now: Date = Date()) -> Date {
        guard let day, !Calendar.current.isDate(day, inSameDayAs: now) else {
            return nextHour(from: now)
        }
        return Calendar.current.date(bySettingHour: 9, minute: 0, second: 0, of: day) ?? day
    }

    /// A new event opens on the next full hour — the time a person most often
    /// means, and never one already gone.
    private static func nextHour(from now: Date = Date()) -> Date {
        let calendar = Calendar.current
        var parts = calendar.dateComponents([.year, .month, .day, .hour], from: now)
        parts.hour = (parts.hour ?? 0) + 1
        return calendar.date(from: parts) ?? now
    }
}

/// Add, rename, recolor and delete calendars, from the button in the
/// CalendarView header. Unlike the event editor there is no draft-and-Save:
/// each row action is its own server write, small enough that the click *is*
/// the explicit save — except delete, which cascades onto the calendar's
/// events and therefore confirms first, stating the count it takes with it.
struct ManageCalendarsSheet: View {
    @EnvironmentObject var store: AppStore
    @Environment(\.dismiss) private var dismiss

    @State private var newName = ""
    @State private var newColor: String?    // nil = the server cycles its enum
    @State private var pendingDelete: EventCalendar?
    @State private var confirmingDelete = false

    private var trimmedNewName: String { newName.trimmingCharacters(in: .whitespacesAndNewlines) }

    var body: some View {
        NavigationStack {
            Form {
                if !store.calendars.isEmpty {
                    Section("Calendars") {
                        ForEach(store.calendars) { calendar in
                            CalendarManageRow(calendar: calendar) {
                                pendingDelete = calendar
                                confirmingDelete = true
                            }
                        }
                    }
                }
                Section("New calendar") {
                    TextField("Name", text: $newName)
                        .onSubmit(add)
                    Picker("Color", selection: $newColor) {
                        // The server cycles its enum for an unnamed color, so
                        // "Automatic" spreads the palette without the user
                        // having to remember what's taken.
                        Text("Automatic").tag(String?.none)
                        ForEach(EventCalendar.colors, id: \.self) { color in
                            Text(color.capitalized).tag(String?.some(color))
                        }
                    }
                    Button("Add Calendar", action: add)
                        .disabled(trimmedNewName.isEmpty)
                }
            }
            .formStyle(.grouped)
            .frame(minWidth: 440, minHeight: 360)
            .navigationTitle("Calendars")
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                }
            }
            .alert("Delete calendar?", isPresented: $confirmingDelete, presenting: pendingDelete) { calendar in
                Button("Delete \"\(calendar.name)\"", role: .destructive) {
                    Task { await store.deleteCalendar(calendar) }
                }
                Button("Cancel", role: .cancel) {}
            } message: { calendar in
                Text("Deleting \"\(calendar.name)\" also deletes \(countPhrase(for: calendar)). This can't be undone.")
            }
        }
    }

    // The cascade is the whole reason this alert exists, so the count is in
    // the sentence, not implied.
    private func countPhrase(for calendar: EventCalendar) -> String {
        let count = store.eventCount(in: calendar)
        return count == 1 ? "its 1 event" : "its \(count) events"
    }

    private func add() {
        let name = trimmedNewName, color = newColor
        guard !name.isEmpty else { return }
        Task {
            if await store.createCalendar(name: name, color: color) {
                newName = ""
                newColor = nil
            }
            // A failed add (dup name, server down) keeps the draft; the error
            // is already on its way to the one shared alert.
        }
    }
}

/// One calendar's row: color popup, rename field, event count, delete. Its
/// own struct because the rename draft is per-calendar @State — identity from
/// ForEach keeps a draft from bleeding across rows.
private struct CalendarManageRow: View {
    @EnvironmentObject var store: AppStore
    let calendar: EventCalendar
    let requestDelete: () -> Void

    @State private var name: String

    init(calendar: EventCalendar, requestDelete: @escaping () -> Void) {
        self.calendar = calendar
        self.requestDelete = requestDelete
        _name = State(initialValue: calendar.name)
    }

    var body: some View {
        HStack(spacing: 10) {
            Circle()
                .fill(Palette.calendar(calendar.color))
                .frame(width: Metric.calendarDot, height: Metric.calendarDot)
            // The same badge the chip row wears: this calendar's events may
            // fire automations. Before the name, so the flexible rename field
            // can't push it out of the eye's path.
            if calendar.isExecutable {
                Image(systemName: "bolt.fill")
                    .font(TypeScale.eventGlyph)
                    .foregroundStyle(Palette.attention)
                    .help("Executable — events here can fire automations")
            }
            // Rename commits on Enter — the row's explicit save. An uncommitted
            // draft simply stays in the field; nothing autosaves on dismiss.
            // labelsHidden, or the grouped Form prints "Name" ahead of every
            // row and shoves the actual name to the trailing edge.
            TextField("Name", text: $name)
                .textFieldStyle(.plain)
                .labelsHidden()
                .multilineTextAlignment(.leading)
                .onSubmit(commitRename)
            Spacer()
            Text(countLabel)
                .font(TypeScale.rowMeta)
                .foregroundStyle(Palette.secondaryText)
            recolorPicker
            Button(action: requestDelete) {
                Image(systemName: "trash")
                    .foregroundStyle(Palette.secondaryText)
            }
            .buttonStyle(.borderless)
            .help("Delete \(calendar.name) and its events")
        }
    }

    private var countLabel: String {
        let count = store.eventCount(in: calendar)
        return count == 1 ? "1 event" : "\(count) events"
    }

    // Recoloring through a binding PATCHes on selection: picking the color is
    // the action, there is nothing else to save with it.
    private var recolorPicker: some View {
        Picker("Color", selection: Binding(
            get: { calendar.color },
            set: { color in Task { await store.updateCalendar(calendar, fields: ["color": color]) } }
        )) {
            ForEach(EventCalendar.colors, id: \.self) { color in
                Text(color.capitalized).tag(color)
            }
            // A color this build doesn't know still has to appear as the
            // current selection, or the picker would render blank and the
            // first click would silently rewrite it.
            if !EventCalendar.colors.contains(calendar.color) {
                Text(calendar.color.capitalized).tag(calendar.color)
            }
        }
        .labelsHidden()
        .fixedSize()
    }

    private func commitRename() {
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty, trimmed != calendar.name else { return }
        Task {
            // On failure (dup name) the draft stays put and the shared alert
            // explains; the row's stored name is still the server's truth.
            _ = await store.updateCalendar(calendar, fields: ["name": trimmed])
        }
    }
}

// MARK: - Projects

/// New project: a name, an optional goal, and — when it was opened from a
/// project's pane — the parent it hangs under. Health, severity, nextDue and
/// the counts are derived server-side from facts and children that do not exist
/// yet, so there is deliberately nothing to set for them here.
struct NewProjectSheet: View {
    @EnvironmentObject var store: AppStore
    @Environment(\.dismiss) private var dismiss
    /// Preset by "New Sub-project"; nil from the sidebar's "+". Fixed rather
    /// than a picker: the pane you opened it from IS the answer, and offering
    /// to change it there would be a second way to do what Edit already does.
    var parent: Project? = nil
    /// Selects the created project, so making one lands you in its pane rather
    /// than back where you started.
    var onCreated: (Project) -> Void = { _ in }

    @State private var name = ""
    @State private var goal = ""
    /// 0 is "set none of my own", which is what a new project usually wants:
    /// the parent's lead is already the right answer, and the stepper says so.
    @State private var defaultLeadDays = 0
    /// "" is None. A new sub-project under an owned parent is already owned by
    /// inheritance, so this starts empty rather than pre-picking the parent's
    /// bot — copying it down would freeze the child against a later handover.
    @State private var ownerBot = ""
    @State private var saving = false

    private var trimmedName: String { name.trimmingCharacters(in: .whitespacesAndNewlines) }

    private var tree: ProjectTree { ProjectTree(store.projects) }

    /// What the project WILL inherit, read off the parent the server has
    /// already rolled up — the same value type the pane header and the Edit
    /// sheet read, so all three say the same thing about the same chain.
    private var inheritance: ProjectInheritance {
        ProjectInheritance(newChildOf: parent, tree: tree, botNames: store.botNames)
    }

    var body: some View {
        NavigationStack {
            // Labelled rows, not a section header per field: a macOS Form puts
            // a TextField's placeholder in the label slot, so a header above one
            // prints the field's name twice the moment it has a value.
            Form {
                Section {
                    TextField("Name", text: $name)
                    if let parent {
                        LabeledContent("Parent", value: parent.name)
                    }
                    // Hidden against a botnetd that predates thresholds, which
                    // would accept the keys and drop them.
                    if tree.supportsThresholds {
                        ProjectThresholdRows(inheritance: inheritance,
                                             defaultLeadDays: $defaultLeadDays,
                                             ownerBot: $ownerBot)
                    }
                }
                Section("Goal") {
                    // A grouped Form prints a TextField's label ahead of the
                    // field and right-aligns what is left, so an empty label
                    // pushes the goal text to the far edge of the box.
                    TextField("", text: $goal, axis: .vertical)
                        .lineLimit(3...8)
                        .labelsHidden()
                        .multilineTextAlignment(.leading)
                }
            }
            .formStyle(.grouped)
            .frame(minWidth: 420, minHeight: 300)
            .navigationTitle(parent == nil ? "New Project" : "New Sub-project")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button(saving ? "Saving…" : "Create") {
                        saving = true
                        Task {
                            let created = await store.createProject(
                                name: trimmedName, goal: cleaned(goal),
                                parentID: parent?.id ?? "",
                                defaultLeadDays: defaultLeadDays, ownerBot: ownerBot)
                            saving = false
                            // A failed create (duplicate name) keeps the sheet
                            // open with the draft; the shared alert explains.
                            if let created {
                                onCreated(created)
                                dismiss()
                            }
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

/// Rename a project, restate its goal, or move it under a different parent.
/// Explicit Save, and only the changed fields go on the wire: projects are
/// last-write-wins like events, so resending an untouched goal would clobber
/// whatever a bot wrote to it while this sheet was open.
struct EditProjectSheet: View {
    @EnvironmentObject var store: AppStore
    @Environment(\.dismiss) private var dismiss

    let project: Project

    @State private var name: String
    @State private var goal: String
    /// "" is None, the top level — the same value the PATCH sends to clear it,
    /// so the picker's selection needs no translation on the way out.
    @State private var parentID: String
    /// 0 is "inherit", and it is what the PATCH sends to clear an own lead —
    /// same no-translation stance as `parentID`.
    @State private var defaultLeadDays: Int
    /// "" is None, which is also what the PATCH sends to clear the owner.
    @State private var ownerBot: String
    @State private var saving = false

    init(project: Project) {
        self.project = project
        _name = State(initialValue: project.name)
        _goal = State(initialValue: project.goal)
        _parentID = State(initialValue: project.parentId ?? "")
        // The project's OWN settings, not the effective ones: an inherited
        // value shown as this project's own would be written back as its own
        // the moment anything else on the sheet changed.
        _defaultLeadDays = State(initialValue: project.defaultLeadDays)
        _ownerBot = State(initialValue: project.ownerBot ?? "")
    }

    private var trimmedName: String { name.trimmingCharacters(in: .whitespacesAndNewlines) }

    private var tree: ProjectTree { ProjectTree(store.projects) }

    /// Rendered with each candidate's depth so two entries at different levels
    /// stay tellable apart; which projects qualify is the tree's own rule.
    private var candidates: [ProjectTree.Row] { tree.parentCandidates(for: project.id) }

    /// Read against the DRAFT parent, not the stored one: with the Parent
    /// picker moved to another project, "inherited (180 d)" has to be what this
    /// project will take after Save, not what it takes now. An unchanged picker
    /// reads the project itself, which also carries its effective values.
    private var inheritance: ProjectInheritance {
        guard parentID != (project.parentId ?? "") else {
            return ProjectInheritance(project: project, tree: tree, botNames: store.botNames)
        }
        return ProjectInheritance(newChildOf: tree.project(parentID), tree: tree,
                                  botNames: store.botNames)
    }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("Name", text: $name)
                    Picker("Parent", selection: $parentID) {
                        Text("None (top level)").tag("")
                        ForEach(candidates) { row in
                            Text(String(repeating: "   ", count: row.depth) + row.project.name)
                                .tag(row.project.id)
                        }
                    }
                    if tree.supportsThresholds {
                        ProjectThresholdRows(inheritance: inheritance,
                                             defaultLeadDays: $defaultLeadDays,
                                             ownerBot: $ownerBot)
                    }
                }
                Section("Goal") {
                    // A grouped Form prints a TextField's label ahead of the
                    // field and right-aligns what is left, so an empty label
                    // pushes the goal text to the far edge of the box.
                    TextField("", text: $goal, axis: .vertical)
                        .lineLimit(3...8)
                        .labelsHidden()
                        .multilineTextAlignment(.leading)
                }
            }
            .formStyle(.grouped)
            .frame(minWidth: 420, minHeight: 300)
            .navigationTitle("Project")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button(saving ? "Saving…" : "Save") {
                        saving = true
                        Task {
                            let saved = await store.updateProject(project, values: changes)
                            saving = false
                            if saved { dismiss() }
                        }
                    }
                    .disabled(saving || trimmedName.isEmpty)
                }
            }
        }
    }

    /// Heterogeneous because `defaultLeadDays` is an int on the wire; the rest
    /// are strings. Only what changed goes, so a bot writing another field
    /// while the sheet was open keeps its write.
    private var changes: [String: Any] {
        var fields: [String: Any] = [:]
        if trimmedName != project.name { fields["name"] = trimmedName }
        let trimmedGoal = goal.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmedGoal != project.goal { fields["goal"] = trimmedGoal }
        // Only on a real move, and "" is how the wire says "back to the top
        // level" — the key's presence is what asks for the change at all.
        if parentID != (project.parentId ?? "") { fields["parentId"] = parentID }
        // 0 and "" are real values here, not omissions: they clear an own
        // setting back to inherited, which is why the comparison is against the
        // project's OWN value and never against the effective one.
        if defaultLeadDays != project.defaultLeadDays {
            fields["defaultLeadDays"] = defaultLeadDays
        }
        if ownerBot != (project.ownerBot ?? "") { fields["ownerBot"] = ownerBot }
        return fields
    }
}

/// The two threshold rows both project sheets show, in one place so the New and
/// Edit forms cannot drift apart on the wording or on what 0/"" mean. The
/// readings come from `ProjectInheritance`, which is where they are proven.
private struct ProjectThresholdRows: View {
    @EnvironmentObject var store: AppStore
    let inheritance: ProjectInheritance
    @Binding var defaultLeadDays: Int
    @Binding var ownerBot: String

    var body: some View {
        // 0 is a legal value here, unlike the Add Fact sheet's per-fact lead:
        // it means "take the one above me", and the label says which number
        // that is rather than printing "0 days".
        Stepper(inheritance.leadStepperText(draft: defaultLeadDays),
                value: $defaultLeadDays, in: 0...365)
            .help("Days before a dated fact's due date that count as due soon. 0 takes the lead from the parent project.")
        Picker("Owner", selection: $ownerBot) {
            Text(inheritance.noneOwnerLabel).tag("")
            // By display name, which is what the tool's `owner` argument takes
            // and the only handle a person has on a bot.
            ForEach(store.bots) { bot in
                Text(bot.displayName).tag(bot.id)
            }
        }
        .help("The bot nudged when this project's health gets worse.")
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
    /// Snapshot-only: open on this kind so the field table's effect on the form
    /// can be rendered — the picker can't be clicked offscreen. Deadline in the
    /// app, which is the kind a project usually starts with.
    var initialKind: FactKind = .deadline

    @State private var kind: FactKind
    @State private var title = ""
    @State private var due = AddFactSheet.defaultDue()
    @State private var leadDays: Int
    @State private var rrule = ""
    @State private var tz = TimeZone.current.identifier
    @State private var blocker = ""
    @State private var body_ = ""
    @State private var saving = false

    init(project: Project, initialKind: FactKind = .deadline) {
        self.project = project
        self.initialKind = initialKind
        _kind = State(initialValue: initialKind)
        // The project's lead, not a flat 30: a fact created with no lead of its
        // own gets exactly this server-side, so the sheet opening on anything
        // else would show a number the fact is not going to have. A botnetd
        // that derives none falls back to the global default it applies.
        _leadDays = State(initialValue: project.hasEffectiveLead
                          ? project.effectiveLeadDays : Project.globalDefaultLeadDays)
    }

    private var trimmedTitle: String { title.trimmingCharacters(in: .whitespacesAndNewlines) }

    /// Recurring REQUIRES an rrule and a tz server-side, so the sheet refuses
    /// to send one without them rather than surfacing the 400.
    private var canSave: Bool {
        guard !saving, !trimmedTitle.isEmpty else { return false }
        guard kind.fields.contains(.rrule) else { return true }
        return !rrule.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && !tz.isEmpty
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
                        TextField("Repeat rule", text: $rrule,
                                  prompt: Text("FREQ=YEARLY;BYMONTH=11;BYMONTHDAY=30"))
                    }
                    if kind.fields.contains(.tz) {
                        TextField("Time zone", text: $tz)
                    }
                    if kind.fields.contains(.blocker) {
                        TextField("Blocker", text: $blocker,
                                  prompt: Text("who or what this waits on"))
                    }
                }
                if kind.fields.contains(.body) {
                    Section(kind == .note ? "Note" : "Details") {
                        // Same label-slot trap as the Goal box above.
                        TextField("", text: $body_, axis: .vertical)
                            .lineLimit(3...8)
                            .labelsHidden()
                            .multilineTextAlignment(.leading)
                    }
                }
            }
            .formStyle(.grouped)
            .frame(minWidth: 460, minHeight: 400)
            .navigationTitle("Add Fact")
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

struct SettingsSheet: View {
    @EnvironmentObject var store: AppStore
    @Environment(\.dismiss) private var dismiss

    @State private var key = ""
    @State private var hasKey = false
    @State private var saving = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    SecureField(hasKey ? "•••• (a key is set)" : "sk-or-…", text: $key)
                        .autocorrectionDisabled()
                } header: {
                    Text("OpenRouter API key")
                } footer: {
                    Text("Sent to botnetd and stored on the server only. The app never keeps it.")
                }
            }
            .formStyle(.grouped)
            .frame(minWidth: 420, minHeight: 200)
            .navigationTitle("Settings")
            .task { hasKey = (try? await store.hasServerKey()) ?? false }
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") {
                        let k = key.trimmingCharacters(in: .whitespacesAndNewlines)
                        saving = true
                        Task {
                            await store.setServerKey(k)
                            dismiss()
                        }
                    }
                    .disabled(saving || key.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                }
            }
        }
    }
}
