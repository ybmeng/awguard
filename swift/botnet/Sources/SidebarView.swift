// SidebarView.swift — the sidebar: search, the services section, and bot rows
// with a preview of the last message. Lives apart from BotNetApp.swift so
// dev/Snapshot can render it without pulling in the @main app.

import SwiftUI

/// What the sidebar has selected. A service is not a bot and has no bot id, so
/// the two are separate cases rather than sentinel strings in a bot id — the
/// detail pane switches on the case and never has to know which ids are real.
enum SidebarSelection: Hashable {
    case bot(String)
    case service(ServiceKind)

    var botID: String? {
        guard case .bot(let id) = self else { return nil }
        return id
    }

    var service: ServiceKind? {
        guard case .service(let kind) = self else { return nil }
        return kind
    }
}

/// The persistently running services the bots share with the user. Calendar is
/// the first with a surface of its own; std_artifacts still lists — the service
/// exists — but has no panel yet, so its row does not select.
enum ServiceKind: String, CaseIterable, Identifiable, Hashable {
    case calendar
    case artifacts

    var id: String { rawValue }

    var title: String {
        switch self {
        case .calendar: return "Calendar"
        case .artifacts: return "std_artifacts"
        }
    }

    var symbol: String {
        switch self {
        case .calendar: return "calendar"
        case .artifacts: return "externaldrive"
        }
    }

    var hasSurface: Bool { self == .calendar }
}

struct SidebarView: View {
    @EnvironmentObject var store: AppStore
    @Binding var selection: SidebarSelection?
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
            servicesSection
            Rectangle().fill(Palette.hairline).frame(height: 1)
            ScrollView {
                LazyVStack(spacing: 2) {
                    ForEach(visibleBots) { bot in
                        BotRow(
                            bot: bot,
                            preview: store.preview(for: bot),
                            stamp: store.lastActivity(for: bot),
                            fallback: modelName(bot.model),
                            selected: selection?.botID == bot.id
                        ) {
                            selection = .bot(bot.id)
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

    // Services sit above the bots because they are shared: every bot writes to
    // the same calendar, so it is not a property of any one conversation.
    private var servicesSection: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text("Services")
                .font(TypeScale.sectionLabel)
                .foregroundStyle(Palette.secondaryText)
                .padding(.horizontal, Metric.sidebarGutter)
                .padding(.bottom, 2)
            ForEach(ServiceKind.allCases) { kind in
                ServiceRow(kind: kind, selected: selection?.service == kind) {
                    selection = .service(kind)
                }
            }
        }
        .padding(.horizontal, Metric.sidebarGutter)
        .padding(.bottom, Metric.sidebarGutter)
    }

    private func modelName(_ id: String) -> String {
        store.models.first(where: { $0.id == id })?.name ?? id
    }
}

// Deliberately built like BotRow rather than shared with it: the two rows agree
// on hover, selection and the leading column width, and on nothing else — a bot
// row carries a preview, a stamp and unread state a service has no analogue for.
private struct ServiceRow: View {
    let kind: ServiceKind
    let selected: Bool
    let select: () -> Void

    @State private var hovering = false

    private var live: Bool { kind.hasSurface }

    var body: some View {
        Button { if live { select() } } label: {
            HStack(spacing: 8) {
                Image(systemName: kind.symbol)
                    .font(TypeScale.serviceIcon)
                    // The same leading width a bot's avatar takes, so service
                    // titles and bot names share one text column.
                    .frame(width: Metric.avatarRow)
                Text(kind.title)
                    .font(TypeScale.rowTitle)
                    .lineLimit(1)
                Spacer(minLength: 0)
            }
            .foregroundStyle(live ? Palette.primaryText : Palette.secondaryText)
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
        .help(live ? "Open \(kind.title)" : "\(kind.title) has no panel yet")
    }

    private var background: Color {
        guard live else { return .clear }
        if selected { return Palette.rowSelected }
        return hovering ? Palette.rowHover : .clear
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
