// BotNetApp.swift — app entry + the three-pane layout from ChatUI.md:
// left nav (Services + Bots), chat panel, bot-details right panel (inspector).

import SwiftUI

@main
struct BotNetApp: App {
    @StateObject private var store = AppStore()

    var body: some Scene {
        WindowGroup {
            ContentView()
                .environmentObject(store)
        }
    }
}

struct ContentView: View {
    @EnvironmentObject var store: AppStore
    @State private var selectedBotID: String?
    @State private var showNewBot = false
    @State private var showSettings = false

    var body: some View {
        NavigationSplitView {
            List(selection: $selectedBotID) {
                Section("Services") {
                    // Services are persistently running apps (e.g. std_artifacts).
                    // Placeholder until service access lands; agents will use these.
                    Label("std_artifacts", systemImage: "externaldrive")
                        .foregroundStyle(.secondary)
                        .selectionDisabled()
                }
                Section("Bots") {
                    ForEach(store.bots) { bot in
                        VStack(alignment: .leading, spacing: 2) {
                            Text(bot.displayName)
                            Text(modelName(bot.model))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                        .tag(bot.id)
                    }
                    .onDelete { idx in
                        idx.map { store.bots[$0] }.forEach { store.deleteBot($0) }
                    }
                }
            }
            .navigationTitle("BotNet")
            .toolbar {
                ToolbarItem(placement: .primaryAction) {
                    Button { showNewBot = true } label: { Image(systemName: "plus") }
                }
                ToolbarItem(placement: .secondaryAction) {
                    Button { showSettings = true } label: { Image(systemName: "gear") }
                }
            }
        } detail: {
            if let id = selectedBotID, let bot = store.bots.first(where: { $0.id == id }) {
                ChatView(bot: bot)
            } else {
                ContentUnavailableView(
                    "No bot selected",
                    systemImage: "bubble.left.and.bubble.right",
                    description: Text("Pick a bot from the sidebar, or create one with +.")
                )
            }
        }
        .sheet(isPresented: $showNewBot) { NewBotSheet() }
        .sheet(isPresented: $showSettings) { SettingsSheet() }
        .alert("Error", isPresented: Binding(
            get: { store.lastError != nil },
            set: { if !$0 { store.lastError = nil } }
        )) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(store.lastError ?? "")
        }
    }

    private func modelName(_ id: String) -> String {
        ModelOption.roster.first(where: { $0.id == id })?.name ?? id
    }
}
