// SidebarView.swift — the conversation list: search, bot rows with a preview of
// the last message, and the services entry. Lives apart from BotNetApp.swift so
// dev/Snapshot can render it without pulling in the @main app.

import SwiftUI

struct SidebarView: View {
    @EnvironmentObject var store: AppStore
    @Binding var selectedBotID: String?
    @Binding var showNewBot: Bool

    @State private var query = ""

    private var visibleBots: [Bot] {
        let needle = query.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !needle.isEmpty else { return store.bots }
        return store.bots.filter { $0.displayName.range(of: needle, options: .caseInsensitive) != nil }
    }

    var body: some View {
        VStack(spacing: 0) {
            searchRow
            ScrollView {
                LazyVStack(spacing: 2) {
                    ForEach(visibleBots) { bot in
                        BotRow(
                            bot: bot,
                            preview: store.preview(for: bot),
                            stamp: store.lastActivity(for: bot),
                            fallback: modelName(bot.model),
                            selected: bot.id == selectedBotID
                        ) {
                            selectedBotID = bot.id
                            Task { await store.markRead(bot) }
                        }
                        .contextMenu {
                            Button("Delete", role: .destructive) {
                                Task { await store.deleteBot(bot) }
                            }
                        }
                    }
                }
                .padding(Metric.sidebarGutter)
            }
            .scrollContentBackground(.hidden)
            Rectangle().fill(Palette.hairline).frame(height: 1)
            servicesRow
            if !store.serverReachable {
                Label("botnetd not reachable", systemImage: "exclamationmark.triangle")
                    .font(TypeScale.rowMeta)
                    .foregroundStyle(Palette.attention)
                    .padding(.horizontal, Metric.sidebarGutter * 2)
                    .padding(.bottom, Metric.sidebarGutter)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .background(Palette.chrome)
    }

    private var searchRow: some View {
        HStack(spacing: 6) {
            HStack(spacing: 6) {
                Image(systemName: "magnifyingglass")
                    .foregroundStyle(Palette.secondaryText)
                TextField("Search", text: $query)
                    .textFieldStyle(.plain)
                    .font(TypeScale.rowPreview)
                    .foregroundStyle(Palette.primaryText)
            }
            .padding(.horizontal, 8)
            .padding(.vertical, 6)
            .background(
                Palette.fieldFill,
                in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
            )

            Button { showNewBot = true } label: {
                Image(systemName: "plus").foregroundStyle(Palette.secondaryText)
            }
            .buttonStyle(.borderless)
            .help("Create a bot")
        }
        .padding(Metric.sidebarGutter)
    }

    // Services are persistently running apps (e.g. std_artifacts); agents will
    // access these. Placeholder until wired.
    private var servicesRow: some View {
        HStack(spacing: 8) {
            Image(systemName: "externaldrive")
            Text("std_artifacts").font(TypeScale.rowPreview)
            Spacer()
        }
        .foregroundStyle(Palette.secondaryText)
        .padding(.horizontal, Metric.sidebarGutter * 2)
        .padding(.vertical, Metric.rowVPad + 2)
    }

    private func modelName(_ id: String) -> String {
        store.models.first(where: { $0.id == id })?.name ?? id
    }
}

private struct BotRow: View {
    let bot: Bot
    let preview: String?
    let stamp: Date?
    let fallback: String
    let selected: Bool
    let select: () -> Void

    @State private var hovering = false

    var body: some View {
        Button(action: select) {
            HStack(spacing: 8) {
                BotAvatar(botID: bot.id, size: Metric.avatarRow)
                VStack(alignment: .leading, spacing: 1) {
                    HStack(spacing: 6) {
                        Text(bot.displayName)
                            .font(TypeScale.rowTitle)
                            .foregroundStyle(Palette.primaryText)
                            .lineLimit(1)
                        Spacer(minLength: 0)
                        if let stamp {
                            Text(Self.stamped(stamp))
                                .font(TypeScale.rowMeta)
                                .foregroundStyle(Palette.secondaryText)
                        }
                    }
                    HStack(spacing: 5) {
                        Text(preview.map(Self.oneLine) ?? fallback)
                            .font(TypeScale.rowPreview)
                            .foregroundStyle(Palette.secondaryText)
                            .lineLimit(1)
                            .truncationMode(.tail)
                        Spacer(minLength: 0)
                        // A broken model is a property of the bot, not of its
                        // last message, so it gets its own quiet glyph rather
                        // than recoloring the preview text.
                        if bot.isModelBroken {
                            Image(systemName: "exclamationmark.triangle.fill")
                                .font(.system(size: 8))
                                .foregroundStyle(Palette.attention)
                        }
                        if bot.hasUnread {
                            Circle()
                                .fill(Palette.attention)
                                .frame(width: 6, height: 6)
                        }
                    }
                }
            }
            .padding(.vertical, Metric.rowVPad)
            .padding(.horizontal, Metric.sidebarGutter)
            .frame(maxWidth: .infinity, alignment: .leading)
            .contentShape(RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous))
            .background(
                background,
                in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
            )
        }
        .buttonStyle(.plain)
        .onHover { hovering = $0 }
    }

    private var background: Color {
        if selected { return Palette.rowSelected }
        return hovering ? Palette.rowHover : .clear
    }

    // Paragraph breaks would otherwise survive as runs of blank space on a row
    // that only ever shows one line.
    private static func oneLine(_ content: String) -> String {
        content
            .components(separatedBy: .whitespacesAndNewlines)
            .filter { !$0.isEmpty }
            .joined(separator: " ")
    }

    private static func stamped(_ date: Date) -> String {
        if Calendar.current.isDateInToday(date) {
            return date.formatted(date: .omitted, time: .shortened)
        }
        return date.formatted(.dateTime.month(.defaultDigits).day())
    }
}
