// Sheets.swift — create-bot and settings sheets. Both write to botnetd; the
// app stores nothing (the OpenRouter key lives on the server).

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
