// BotNetApp.swift — app entry + the two-pane layout: the conversation sidebar
// and the chat panel. All state comes from botnetd.

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
    @State private var showDetails = false

    var body: some View {
        NavigationSplitView {
            SidebarView(selectedBotID: $selectedBotID, showNewBot: $showNewBot)
                .navigationSplitViewColumnWidth(min: 240, ideal: Metric.sidebarWidth, max: 380)
                .toolbar {
                    ToolbarItem {
                        Button { showSettings = true } label: { Image(systemName: "gear") }
                            .help("Settings")
                    }
                }
        } detail: {
            if let id = selectedBotID, let bot = store.bots.first(where: { $0.id == id }) {
                ChatView(bot: bot, showDetails: $showDetails)
                    .inspector(isPresented: $showDetails) {
                        BotDetails(bot: bot, messageCount: store.messages(for: bot.id).count)
                    }
            } else {
                ContentUnavailableView(
                    "No bot selected",
                    systemImage: "bubble.left.and.bubble.right",
                    description: Text("Pick a bot, or create one with + next to Search.")
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
}
