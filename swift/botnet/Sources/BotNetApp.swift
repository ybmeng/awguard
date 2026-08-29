// BotNetApp.swift — app entry + the layout from ChatUI.md: left nav (Services +
// Bots), chat panel, bot-details inspector. All state comes from botnetd.

import SwiftUI

@main
struct BotNetApp: App {
    @StateObject private var store = AppStore()

    var body: some Scene {
        WindowGroup {
            ContentView()
                .environmentObject(store)
                .task { await store.refresh() }
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
                    // Services are persistently running apps (e.g. std_artifacts);
                    // agents will access these. Placeholder until wired.
                    Label("std_artifacts", systemImage: "externaldrive")
                        .foregroundStyle(.secondary)
                }
                Section {
                    ForEach(store.bots) { bot in
                        VStack(alignment: .leading, spacing: 2) {
                            Text(bot.displayName)
                            Text(modelName(bot.model))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                        .tag(bot.id)
                        .contextMenu {
                            Button("Delete", role: .destructive) {
                                Task { await store.deleteBot(bot) }
                            }
                        }
                    }
                } header: {
                    // Create button anchored to the right of the "Bots" header.
                    HStack {
                        Text("Bots")
                        Spacer()
                        Button {
                            showNewBot = true
                        } label: {
                            Image(systemName: "plus")
                        }
                        .buttonStyle(.borderless)
                        .help("Create a bot")
                    }
                }
            }
            .navigationTitle("BotNet")
            .toolbar {
                ToolbarItem {
                    Button { showSettings = true } label: { Image(systemName: "gear") }
                        .help("Settings")
                }
            }
            .safeAreaInset(edge: .bottom) {
                if !store.serverReachable {
                    Label("botnetd not reachable", systemImage: "exclamationmark.triangle")
                        .font(.caption)
                        .foregroundStyle(.orange)
                        .padding(6)
                }
            }
        } detail: {
            if let id = selectedBotID, let bot = store.bots.first(where: { $0.id == id }) {
                ChatView(bot: bot)
            } else {
                ContentUnavailableView(
                    "No bot selected",
                    systemImage: "bubble.left.and.bubble.right",
                    description: Text("Pick a bot, or create one with + next to Bots.")
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
        store.models.first(where: { $0.id == id })?.name ?? id
    }
}
