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
