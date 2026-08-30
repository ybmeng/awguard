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
    case existing(Event)

    var id: String {
        switch self {
        case .new: return "new"
        case .existing(let event): return event.id
        }
    }

    var event: Event? {
        guard case .existing(let event) = self else { return nil }
        return event
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
    @State private var saving = false

    init(target: EventTarget) {
        self.target = target
        let event = target.event
        let start = event?.startsAt ?? Self.nextHour()
        _title = State(initialValue: event?.title ?? "")
        _starts = State(initialValue: start)
        _ends = State(initialValue: event?.endsAt ?? start.addingTimeInterval(Self.defaultDuration))
        _location = State(initialValue: event?.location ?? "")
        _notes = State(initialValue: event?.notes ?? "")
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
            case .new:
                saved = await store.createEvent(
                    title: trimmedTitle, startsAt: starts, endsAt: ends,
                    location: cleaned(location), notes: cleaned(notes))
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
        return fields
    }

    private func cleaned(_ text: String) -> String {
        text.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private static let defaultDuration: TimeInterval = 3600

    /// A new event opens on the next full hour — the time a person most often
    /// means, and never one already gone.
    private static func nextHour(from now: Date = Date()) -> Date {
        let calendar = Calendar.current
        var parts = calendar.dateComponents([.year, .month, .day, .hour], from: now)
        parts.hour = (parts.hour ?? 0) + 1
        return calendar.date(from: parts) ?? now
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
