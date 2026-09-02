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
    @State private var selection: SidebarSelection?
    @State private var showNewBot = false
    @State private var showSettings = false
    @State private var showDetails = false
    // Lives here rather than in BotDetails so the choice survives the inspector
    // closing and reopening within one run.
    @State private var memoryExpanded = true
    // Same reasoning as memoryExpanded: the calendar's list/grid choice belongs
    // to the window, not to a CalendarView that is rebuilt on every selection.
    @State private var calendarMode: CalendarMode = .list

    var body: some View {
        NavigationSplitView {
            SidebarView(selection: $selection, showNewBot: $showNewBot)
                .navigationSplitViewColumnWidth(min: 240, ideal: Metric.sidebarWidth, max: 380)
                .toolbar {
                    ToolbarItem {
                        Button { showSettings = true } label: { Image(systemName: "gear") }
                            .help("Settings")
                    }
                }
        } detail: {
            // The sidebar's two-case selection is what picks the surface; the
            // bot is resolved from the live list here (never captured) so a
            // refetch propagates and a deleted bot falls back to the placeholder.
            switch selection {
            case .bot(let id)?:
                if let bot = store.bots.first(where: { $0.id == id }) {
                    ChatView(bot: bot, showDetails: $showDetails)
                        .inspector(isPresented: $showDetails) {
                            BotDetails(bot: bot, expanded: $memoryExpanded)
                        }
                } else {
                    nothingSelected
                }
            case .service(.calendar)?:
                CalendarView(mode: $calendarMode)
            case .automation(let name)?:
                // Same live-resolve rule as bots: the automation comes off
                // store.automations at render time, so a refresh moves the
                // freshness badge and a vanished automation (manifest deleted,
                // repo rescanned) falls back rather than rendering a ghost.
                if let automation = store.automations.first(where: { $0.name == name }) {
                    AutomationView(automation: automation)
                } else {
                    nothingSelected
                }
            case .service(.artifacts)?, nil:
                nothingSelected
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

    private var nothingSelected: some View {
        ContentUnavailableView(
            "Nothing selected",
            systemImage: "bubble.left.and.bubble.right",
            description: Text("Pick a bot or a service, or create a bot with + next to Search.")
        )
    }
}
