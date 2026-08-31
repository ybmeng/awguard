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
    @State private var showManage = false
    // Which calendar the pane is narrowed to; nil is "All". Deliberately
    // transient @State: a filter is a glance, not a setting, so leaving the
    // panel clears it. The init parameters exist for the offscreen snapshot
    // tool, which has no way to click a chip or type.
    @State private var filter: String?
    // The header's type-to-filter text; transient for the same reason.
    @State private var query = ""

    init(mode: Binding<CalendarMode>, initialFilter: String? = nil, initialQuery: String = "") {
        _mode = mode
        _filter = State(initialValue: initialFilter)
        _query = State(initialValue: initialQuery)
    }

    // One filtered array feeds both readings, so the list and the grid can
    // never disagree about what the active chip and the typed text mean. The
    // two filters compose: an event has to satisfy both.
    private var filteredEvents: [Event] {
        var events = store.events
        if let filter { events = events.filter { $0.calendarId == filter } }
        let needle = query.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !needle.isEmpty else { return events }
        return events.filter { matches($0, needle: needle) }
    }

    // Substring, case-insensitive, across everything a person remembers an
    // event by — including its calendar's name, so typing "earnings" narrows
    // to that calendar without hunting for its chip.
    private func matches(_ event: Event, needle: String) -> Bool {
        if event.title.range(of: needle, options: .caseInsensitive) != nil { return true }
        if event.location?.range(of: needle, options: .caseInsensitive) != nil { return true }
        if event.notes?.range(of: needle, options: .caseInsensitive) != nil { return true }
        if let name = store.calendar(id: event.calendarId)?.name,
           name.range(of: needle, options: .caseInsensitive) != nil { return true }
        return false
    }

    private var groups: EventGroups { EventGroups(filteredEvents) }

    var body: some View {
        VStack(spacing: 0) {
            header
            if !store.calendars.isEmpty { chipRow }
            switch mode {
            case .list:
                if filteredEvents.isEmpty { empty } else { list }
            case .month:
                // A month with nothing in it is still a month, so the grid has
                // no empty state of its own.
                MonthGridView(events: filteredEvents) { editing = $0 }
            }
        }
        .background(Palette.chrome)
        // The calendar is shared state that a bot can change between visits, so
        // opening the panel re-reads it rather than trusting the launch fetch.
        // (refreshEvents re-reads the calendars too.)
        .task { await store.refreshEvents() }
        // A filter pointing at a calendar that no longer exists would silently
        // show nothing; fall back to All the moment the calendar goes.
        .onChange(of: store.calendars) {
            if let filter, !store.calendars.contains(where: { $0.id == filter }) {
                self.filter = nil
            }
        }
        .sheet(item: $editing) { EventSheet(target: $0, defaultCalendarId: defaultCalendarId) }
        .sheet(isPresented: $showManage) { ManageCalendarsSheet() }
    }

    /// Where a new event files by default: the filtered calendar when one chip
    /// is active, else "Personal" when it exists, else nil — the sheet then
    /// omits calendarId and the server Personal-ensures, per the contract.
    private var defaultCalendarId: String? {
        filter ?? store.calendars.first {
            $0.name.caseInsensitiveCompare("Personal") == .orderedSame
        }?.id
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
            searchField
            modePicker
            // Hidden on a botnetd that predates multiple calendars: the sheet
            // could only fail there, and the pane should read exactly as it
            // did before the feature.
            if !store.calendarsUnavailable {
                Button { showManage = true } label: {
                    Image(systemName: "slider.horizontal.3")
                        .foregroundStyle(Palette.secondaryText)
                }
                .buttonStyle(.borderless)
                .help("Manage calendars")
            }
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

    // The same dress as the sidebar's bot search, because it is the same
    // gesture: type, watch the list narrow live. Nothing debounces — the
    // events are already in memory.
    private var searchField: some View {
        HStack(spacing: 5) {
            Image(systemName: "magnifyingglass")
                .foregroundStyle(Palette.secondaryText)
            TextField("Search", text: $query)
                .textFieldStyle(.plain)
                .font(TypeScale.rowPreview)
                .foregroundStyle(Palette.primaryText)
            if !query.isEmpty {
                Button { query = "" } label: {
                    Image(systemName: "xmark.circle.fill")
                        .foregroundStyle(Palette.secondaryText)
                }
                .buttonStyle(.plain)
                .help("Clear the search")
            }
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 5)
        .background(
            Palette.fieldFill,
            in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
        )
        .frame(width: Metric.calendarSearchWidth)
        .help("Filter events by title, location, notes, or calendar")
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

    /// How many calendar chips draw inline before the rest fold into the
    /// "more" menu. Six fits comfortably at the pane's narrowest sensible
    /// width; a dozen-plus calendars must not become a wall of chips.
    private static let maxInlineChips = 6

    // The chips actually drawn. Past the cap, the active calendar is swapped
    // into the last inline slot so the selection is always visible — a filter
    // hidden inside the menu would make the narrowed list unexplainable.
    private var inlineCalendars: [EventCalendar] {
        let all = store.calendars
        guard all.count > Self.maxInlineChips else { return all }
        var head = Array(all.prefix(Self.maxInlineChips))
        if let filter, !head.contains(where: { $0.id == filter }),
           let active = all.first(where: { $0.id == filter }) {
            head[head.count - 1] = active
        }
        return head
    }

    private var overflowCalendars: [EventCalendar] {
        let all = store.calendars
        guard all.count > Self.maxInlineChips else { return [] }
        let inline = Set(inlineCalendars.map(\.id))
        return all.filter { !inline.contains($0.id) }
    }

    // The filter row: "All" plus one chip per calendar, folding into a menu
    // past the inline cap. The scroll wrapper is a safety net for long names,
    // not the overflow strategy; selection is single, and "All" clears it.
    private var chipRow: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 6) {
                FilterChip(label: "All", color: nil, selected: filter == nil) {
                    filter = nil
                }
                ForEach(inlineCalendars) { calendar in
                    FilterChip(
                        label: calendar.name,
                        color: Palette.calendar(calendar.color),
                        selected: filter == calendar.id
                    ) {
                        filter = calendar.id
                    }
                }
                if !overflowCalendars.isEmpty { overflowMenu }
            }
            .padding(.horizontal, Metric.transcriptHPad)
            .padding(.vertical, 8)
        }
        .overlay(alignment: .bottom) {
            Rectangle().fill(Palette.hairline).frame(height: 1)
        }
    }

    // Dressed as a chip so the row reads as one control. It is never in the
    // selected state: choosing from it swaps that calendar inline.
    private var overflowMenu: some View {
        Menu {
            ForEach(overflowCalendars) { calendar in
                Button(calendar.name) { filter = calendar.id }
            }
        } label: {
            HStack(spacing: 4) {
                Text("\(overflowCalendars.count) more")
                Image(systemName: "chevron.down").font(TypeScale.gridMeta)
            }
            .font(TypeScale.rowMeta)
            .foregroundStyle(Palette.primaryText)
            .padding(.horizontal, 9)
            .padding(.vertical, 4)
            .background(Palette.fieldFill, in: Capsule())
        }
        .menuStyle(.button)
        .buttonStyle(.plain)
        .menuIndicator(.hidden)
        .fixedSize()
        .help("Filter by one of the remaining calendars")
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
                EventRow(event: event, creator: creator(of: event),
                         calendarColor: calendarColor(of: event)) {
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

    // An empty list means two different things: nothing on the calendar, or
    // filters that nothing survives. Saying "Add one with +" to a search miss
    // would send the user creating an event they were looking for.
    private var isNarrowed: Bool {
        filter != nil || !query.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    @ViewBuilder
    private var empty: some View {
        if isNarrowed {
            ContentUnavailableView {
                Label("No matching events", systemImage: "magnifyingglass")
            } description: {
                Text("Nothing matches the current filters. Clear the search or switch to All.")
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        } else {
            ContentUnavailableView {
                Label("No events", systemImage: ServiceKind.calendar.symbol)
            } description: {
                Text("Add one with +. Bots write to this calendar too — ask one to put something on it.")
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    /// The event's calendar color, resolved against the live calendar list so a
    /// recolor propagates without a refetch. Nil — no dot at all — when the
    /// event has no calendar (old server) or its calendar is gone; an unknown
    /// color *string* still dots, in the palette's neutral fallback.
    private func calendarColor(of event: Event) -> Color? {
        store.calendar(id: event.calendarId).map { Palette.calendar($0.color) }
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

/// One capsule in the filter row. The color dot is the same one the rows and
/// grid chips carry, so the chip reads as a legend for the colors below it;
/// the selected chip uses the app's solid pair, the same "this one is active"
/// treatment as today's date marker.
private struct FilterChip: View {
    let label: String
    let color: Color?
    let selected: Bool
    let select: () -> Void

    var body: some View {
        Button(action: select) {
            HStack(spacing: 5) {
                if let color {
                    Circle().fill(color)
                        .frame(width: Metric.calendarDot, height: Metric.calendarDot)
                }
                Text(label).lineLimit(1)
            }
            .font(TypeScale.rowMeta)
            .foregroundStyle(selected ? Palette.userBubbleText : Palette.primaryText)
            .padding(.horizontal, 9)
            .padding(.vertical, 4)
            .background(selected ? Palette.userBubble : Palette.fieldFill, in: Capsule())
        }
        .buttonStyle(.plain)
        .help(color == nil ? "Show every calendar" : "Show only \(label)")
    }
}

private struct EventRow: View {
    let event: Event
    let creator: EventCreator
    let calendarColor: Color?
    let open: () -> Void

    @State private var hovering = false

    var body: some View {
        Button(action: open) {
            HStack(alignment: .top, spacing: 10) {
                time
                VStack(alignment: .leading, spacing: 2) {
                    HStack(spacing: 6) {
                        // The calendar's color, right where the eye already is.
                        // A dot rather than a tint: six tinted rows would turn
                        // the list into a heat map, and a dot reads identically
                        // on the light and dark grounds.
                        if let calendarColor {
                            Circle().fill(calendarColor)
                                .frame(width: Metric.calendarDot, height: Metric.calendarDot)
                        }
                        Text(event.title)
                            .font(TypeScale.rowTitle)
                            .foregroundStyle(Palette.primaryText)
                            .lineLimit(1)
                    }
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
