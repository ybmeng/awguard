// CalendarView.swift — the Calendar service's panel. One list of every event
// the server holds, grouped by day and led by what is still ahead. Bots write
// to the same calendar the user does, so each row also says who put it there;
// that attribution is the whole reason this is a service and not a local store.

import SwiftUI

/// The Calendar surface's two readings of the same events: the list, which
/// answers "what is next", and the grid, which answers "what does this month
/// look like". List is the default; the choice is owned by ContentView so it
/// survives leaving the panel and coming back within a run.
enum CalendarMode: String, CaseIterable, Identifiable {
    case list, month

    var id: String { rawValue }

    var symbol: String {
        switch self {
        case .list: return "list.bullet"
        case .month: return "square.grid.3x3"
        }
    }
}

struct CalendarView: View {
    @EnvironmentObject var store: AppStore
    @Binding var mode: CalendarMode

    @State private var editing: EventTarget?

    private var groups: EventGroups { EventGroups(store.events) }

    var body: some View {
        VStack(spacing: 0) {
            header
            switch mode {
            case .list:
                if store.events.isEmpty { empty } else { list }
            case .month:
                // A month with nothing in it is still a month, so the grid has
                // no empty state of its own.
                MonthGridView { editing = $0 }
            }
        }
        .background(Palette.chrome)
        // The calendar is shared state that a bot can change between visits, so
        // opening the panel re-reads it rather than trusting the launch fetch.
        .task { await store.refreshEvents() }
        .sheet(item: $editing) { EventSheet(target: $0) }
    }

    private var header: some View {
        HStack(spacing: 8) {
            Image(systemName: ServiceKind.calendar.symbol)
                .font(TypeScale.serviceIcon)
                .foregroundStyle(Palette.primaryText)
            Text(ServiceKind.calendar.title)
                .font(TypeScale.headerTitle)
                .foregroundStyle(Palette.primaryText)
            Spacer()
            modePicker
            Button { editing = .new } label: {
                Image(systemName: "plus").foregroundStyle(Palette.secondaryText)
            }
            .buttonStyle(.borderless)
            .help("New event")
        }
        .padding(.horizontal, Metric.transcriptHPad)
        .frame(height: Metric.headerHeight)
        .overlay(alignment: .bottom) {
            Rectangle().fill(Palette.hairline).frame(height: 1)
        }
    }

    // Icons only: two words up here would compete with the title, and the
    // segmented control is the platform's own compact switch.
    private var modePicker: some View {
        Picker("View", selection: $mode) {
            ForEach(CalendarMode.allCases) { mode in
                Image(systemName: mode.symbol).tag(mode)
            }
        }
        .pickerStyle(.segmented)
        .labelsHidden()
        .fixedSize()
        .help("Switch between the list and the month grid")
    }

    private var list: some View {
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 0) {
                ForEach(Array(groups.upcoming.enumerated()), id: \.element.id) { index, day in
                    dayGroup(day).padding(.top, index == 0 ? 0 : Metric.dayGroupGap)
                }
                if !groups.past.isEmpty {
                    Text("Earlier")
                        .font(TypeScale.sectionLabel)
                        .foregroundStyle(Palette.secondaryText)
                        .padding(.horizontal, Metric.sidebarGutter)
                        .padding(.top, groups.upcoming.isEmpty ? 0 : Metric.dayGroupGap)
                    ForEach(groups.past) { day in
                        dayGroup(day).padding(.top, Metric.dayGroupGap)
                    }
                }
            }
            .frame(maxWidth: Metric.calendarListWidth, alignment: .leading)
            .padding(.horizontal, Metric.transcriptHPad)
            .padding(.vertical, Metric.transcriptVPad)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func dayGroup(_ day: EventDay) -> some View {
        VStack(alignment: .leading, spacing: Metric.eventRowGap) {
            Text(day.label)
                .font(TypeScale.dayHeader)
                .foregroundStyle(day.isPast ? Palette.secondaryText : Palette.primaryText)
                .padding(.horizontal, Metric.sidebarGutter)
                .padding(.bottom, 2)
            ForEach(day.events) { event in
                EventRow(event: event, creator: creator(of: event)) {
                    editing = .existing(event)
                }
                .contextMenu {
                    Button("Delete", role: .destructive) {
                        Task { await store.deleteEvent(event) }
                    }
                }
            }
        }
    }

    private var empty: some View {
        ContentUnavailableView {
            Label("No events", systemImage: ServiceKind.calendar.symbol)
        } description: {
            Text("Add one with +. Bots write to this calendar too — ask one to put something on it.")
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    // Resolved against the live bot list at render time, never captured onto the
    // row, so a renamed bot renames here without a refetch.
    private func creator(of event: Event) -> EventCreator {
        guard !event.isUserCreated else { return .user }
        let name = store.bots.first { $0.id == event.createdBy }?.displayName
        return .bot(id: event.createdBy, name: name ?? "a bot")
    }
}

/// Who put an event on the calendar.
enum EventCreator {
    case user
    /// A bot that may no longer be in the list, so the name is resolved, not
    /// assumed — a deleted bot's events stay on the calendar.
    case bot(id: String, name: String)
}

private struct EventRow: View {
    let event: Event
    let creator: EventCreator
    let open: () -> Void

    @State private var hovering = false

    var body: some View {
        Button(action: open) {
            HStack(alignment: .top, spacing: 10) {
                time
                VStack(alignment: .leading, spacing: 2) {
                    Text(event.title)
                        .font(TypeScale.rowTitle)
                        .foregroundStyle(Palette.primaryText)
                        .lineLimit(1)
                    if event.hasLocation {
                        Label(event.location ?? "", systemImage: "mappin.and.ellipse")
                            .font(TypeScale.rowMeta)
                            .foregroundStyle(Palette.secondaryText)
                            .lineLimit(1)
                    }
                }
                Spacer(minLength: 8)
                author
            }
            .padding(.vertical, Metric.rowVPad)
            .padding(.horizontal, Metric.sidebarGutter)
            .frame(maxWidth: .infinity, alignment: .leading)
            .contentShape(RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous))
            .background(
                hovering ? Palette.rowHover : .clear,
                in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
            )
        }
        .buttonStyle(.plain)
        .onHover { hovering = $0 }
        .help("Edit \(event.title)")
    }

    // A fixed column so every title on the day starts at the same x; the end
    // time is quieter because the start is what the eye scans for.
    private var time: some View {
        VStack(alignment: .trailing, spacing: 1) {
            Text(Self.clock(event.startsAt))
                .foregroundStyle(Palette.primaryText)
            Text(Self.endClock(event))
                .foregroundStyle(Palette.secondaryText)
        }
        .font(TypeScale.rowMeta)
        .lineLimit(1)
        .frame(width: Metric.eventTimeWidth, alignment: .trailing)
    }

    private var author: some View {
        HStack(spacing: 5) {
            switch creator {
            case .user:
                Image(systemName: "person.crop.circle")
                Text("you")
            case .bot(let id, let name):
                BotAvatar(botID: id, size: Metric.avatarSmall)
                Text(name)
            }
        }
        .font(TypeScale.rowMeta)
        .foregroundStyle(Palette.secondaryText)
        .lineLimit(1)
    }

    private static func clock(_ date: Date) -> String {
        date.formatted(date: .omitted, time: .shortened)
    }

    // An overnight flight files under the day it leaves, so its end time would
    // otherwise read as twelve hours before its start. The day count says so.
    private static func endClock(_ event: Event) -> String {
        let end = Calendar.current.startOfDay(for: event.endsAt)
        let days = Calendar.current.dateComponents([.day], from: event.day, to: end).day ?? 0
        return days > 0 ? "\(clock(event.endsAt)) +\(days)" : clock(event.endsAt)
    }
}

// MARK: - grouping

/// The event list as it reads: the days still ahead in order, then the days
/// already gone, most recent first. Leading with last month would make the
/// panel useless; dropping it would make it a lie.
struct EventGroups {
    var upcoming: [EventDay] = []
    var past: [EventDay] = []

    init(_ events: [Event], now: Date = Date()) {
        let today = Calendar.current.startOfDay(for: now)
        let byDay = Dictionary(grouping: events, by: \.day)
        let days = byDay.map { date, events in
            EventDay(date: date, events: events.sorted {
                ($0.startsAt, $0.id) < ($1.startsAt, $1.id)
            }, today: today)
        }
        upcoming = days.filter { !$0.isPast }.sorted { $0.date < $1.date }
        past = days.filter(\.isPast).sorted { $0.date > $1.date }
    }
}

struct EventDay: Identifiable {
    let date: Date
    let events: [Event]
    let isPast: Bool
    let label: String

    var id: Date { date }

    init(date: Date, events: [Event], today: Date) {
        self.date = date
        self.events = events
        self.isPast = date < today
        self.label = Self.label(for: date, today: today)
    }

    // "Today · Sun, Aug 30" — the relative word is what the user is actually
    // looking for, and the date behind it keeps the list unambiguous. The year
    // appears only when it isn't this one, so the common case stays short.
    private static func label(for date: Date, today: Date) -> String {
        let calendar = Calendar.current
        let sameYear = calendar.component(.year, from: date) == calendar.component(.year, from: today)
        let stamp = sameYear
            ? date.formatted(.dateTime.weekday(.abbreviated).month(.abbreviated).day())
            : date.formatted(.dateTime.weekday(.abbreviated).month(.abbreviated).day().year())
        switch calendar.dateComponents([.day], from: today, to: date).day ?? 0 {
        case 0: return "Today · \(stamp)"
        case 1: return "Tomorrow · \(stamp)"
        case -1: return "Yesterday · \(stamp)"
        default: return stamp
        }
    }
}
