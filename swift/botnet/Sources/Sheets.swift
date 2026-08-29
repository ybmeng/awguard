// Sheets.swift — create-bot and settings (OpenRouter key) sheets.

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
                        ForEach(ModelOption.roster) { opt in
                            Text(opt.name).tag(opt.id)
                        }
                    }
                    .pickerStyle(.inline)
                    .labelsHidden()
                }
                Section("System prompt") {
                    TextField("You are…", text: $systemPrompt, axis: .vertical)
                        .lineLimit(3...10)
                }
            }
            .navigationTitle("New Bot")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Create") {
                        store.createBot(displayName: name, systemPrompt: systemPrompt, model: model)
                        dismiss()
                    }
                    .disabled(name.trimmingCharacters(in: .whitespaces).isEmpty)
                }
            }
        }
    }
}

struct SettingsSheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var key = Keychain.apiKey

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    SecureField("sk-or-…", text: $key)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                } header: {
                    Text("OpenRouter API key")
                } footer: {
                    Text("Stored in the Keychain on this device only.")
                }
            }
            .navigationTitle("Settings")
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") {
                        Keychain.apiKey = key.trimmingCharacters(in: .whitespacesAndNewlines)
                        dismiss()
                    }
                }
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
            }
        }
    }
}
