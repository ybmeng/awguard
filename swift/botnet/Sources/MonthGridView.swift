// MonthGridView.swift — the Calendar service's second reading of the same
// events. The list answers "what is next"; the grid answers "what does this
// month look like", which is the question a list can't answer at a glance.
//
// The grid owns no data and no sheet: it hands an EventTarget back to
// CalendarView, which presents the one editor both views share.

import SwiftUI

struct MonthGridView: View {
    @EnvironmentObject var store: AppStore
    /// Called with the event to edit, or the day to create one on.
    let open: (EventTarget) -> Void

    private var months: [MonthGrid] { MonthGrid.range(covering: store.events) }
    private var byDay: [Date: [Event]] { Dictionary(grouping: store.events, by: \.day) }

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                // Deliberately not lazy: scrollTo cannot reach a section a
                // LazyVStack has not built yet, and a calendar is a handful of
                // months, not a transcript.
                VStack(alignment: .leading, spacing: Metric.dayGroupGap) {
                    ForEach(months) { month in
                        section(month).id(month.id)
                    }
                }
                .padding(.horizontal, Metric.transcriptHPad)
                .padding(.vertical, Metric.transcriptVPad)
            }
            // The range runs back through the oldest event; the month the user
            // means is this one.
            .onAppear { proxy.scrollTo(MonthGrid.month(of: Date()), anchor: .top) }
        }
    }

    private func section(_ month: MonthGrid) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(month.title)
                .font(TypeScale.monthHeader)
                .foregroundStyle(Palette.primaryText)
            weekdayHeader
            grid(month)
        }
    }

    private var weekdayHeader: some View {
        HStack(spacing: Metric.monthCellGap) {
            ForEach(Array(MonthGrid.weekdaySymbols.enumerated()), id: \.offset) { _, symbol in
                Text(symbol)
                    .font(TypeScale.gridMeta)
                    .foregroundStyle(Palette.secondaryText)
                    .frame(maxWidth: .infinity)
            }
        }
    }

    // The rules between cells are the grid's own background showing through the
    // one-point gaps, so there are no divider views to keep aligned.
    private func grid(_ month: MonthGrid) -> some View {
        VStack(spacing: Metric.monthCellGap) {
            ForEach(Array(month.weeks.enumerated()), id: \.offset) { _, week in
                HStack(spacing: Metric.monthCellGap) {
                    ForEach(week, id: \.self) { day in
                        let inMonth = month.contains(day)
                        DayCell(
                            day: day,
                            inMonth: inMonth,
                            // A trailing cell's events belong to the next
                            // month's section; drawing them twice would make
                            // the same event look like two.
                            events: inMonth ? (byDay[day] ?? []) : [],
                            open: open
                        )
                    }
                }
            }
        }
        .padding(Metric.monthCellGap)
        .background(Palette.hairline)
    }
}

private struct DayCell: View {
    let day: Date
    let inMonth: Bool
    let events: [Event]
    let open: (EventTarget) -> Void

    @State private var hovering = false

    /// A cell shows this many chips and then counts the rest. A fourth chip
    /// does not fit the cell's height, and a cell that scrolled internally
    /// would fight the month list scrolling around it.
    private static let maxChips = 3

    private var visible: [Event] { Array(events.prefix(Self.maxChips)) }
    private var overflow: Int { events.count - visible.count }
    // Gated on inMonth so today is marked once, in its own month's section,
    // not again on a neighbouring section's muted filler cell.
    private var isToday: Bool { inMonth && Calendar.current.isDateInToday(day) }

    var body: some View {
        ZStack(alignment: .topLeading) {
            // The empty area of the cell is the "add an event here" target. It
            // sits under the labels, which opt out of hit testing so the click
            // reaches it; the chips keep theirs and open their own event.
            Button { open(.newOn(day)) } label: {
                Rectangle().fill(background).contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .disabled(!inMonth)
            .help(inMonth ? "Add an event on \(day.formatted(date: .abbreviated, time: .omitted))" : "")

            VStack(alignment: .leading, spacing: 1) {
                dayNumber
                ForEach(visible) { event in
                    EventChip(event: event) { open(.existing(event)) }
                }
                if overflow > 0 {
                    Text("+\(overflow) more")
                        .font(TypeScale.gridMeta)
                        .foregroundStyle(Palette.secondaryText)
                        .padding(.horizontal, Metric.chipHPad)
                        .allowsHitTesting(false)
                }
            }
            .padding(Metric.chipVPad + 1)
            .frame(maxHeight: .infinity, alignment: .top)
        }
        .frame(height: Metric.monthCellHeight)
        .onHover { hovering = $0 }
    }

    // The whole row opts out of hit testing, so the hover "+" is an affordance
    // for the click the cell behind it already accepts, not a second button.
    private var dayNumber: some View {
        HStack(spacing: 2) {
            // Today gets the app's solid pair — the same one the user's own
            // bubble uses — because a row-tint circle on the cell's own ground
            // is invisible at this size. It inverts correctly in dark mode.
            Text(day.formatted(.dateTime.day()))
                .font(TypeScale.dayNumber)
                .foregroundStyle(numberColor)
                .frame(width: Metric.todayMarker, height: Metric.todayMarker)
                .background(isToday ? Palette.userBubble : .clear, in: Circle())
            Spacer(minLength: 0)
            if hovering && inMonth {
                Image(systemName: "plus")
                    .font(TypeScale.gridMeta)
                    .foregroundStyle(Palette.secondaryText)
                    .padding(.trailing, 3)
            }
        }
        .allowsHitTesting(false)
    }

    private var numberColor: Color {
        if isToday { return Palette.userBubbleText }
        return inMonth ? Palette.primaryText : Palette.secondaryText
    }

    private var background: Color {
        guard inMonth else { return Palette.fieldFill }
        return hovering ? Palette.rowHover : Palette.chrome
    }
}

private struct EventChip: View {
    let event: Event
    let open: () -> Void

    var body: some View {
        Button(action: open) {
            HStack(spacing: 3) {
                Text(Self.clock(event.startsAt))
                    .foregroundStyle(Palette.secondaryText)
                Text(event.title)
                    .foregroundStyle(Palette.primaryText)
                    .lineLimit(1)
                Spacer(minLength: 0)
            }
            .font(TypeScale.chip)
            .padding(.horizontal, Metric.chipHPad)
            .padding(.vertical, Metric.chipVPad)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(
                Palette.rowSelected,
                in: RoundedRectangle(cornerRadius: Metric.chipRadius, style: .continuous)
            )
        }
        .buttonStyle(.plain)
        .help("\(event.title) — \(Self.range(event))")
    }

    /// Compact enough for a cell barely a hundred points wide: "2p" and "9:30a"
    /// in a 12-hour locale, "14" and "9:30" in a 24-hour one, where the narrow
    /// am/pm symbol is empty. The full range is in the tooltip.
    private static func clock(_ date: Date) -> String {
        let onTheHour = Calendar.current.component(.minute, from: date) == 0
        return onTheHour
            ? date.formatted(.dateTime.hour(.defaultDigits(amPM: .narrow)))
            : date.formatted(.dateTime.hour(.defaultDigits(amPM: .narrow)).minute())
    }

    private static func range(_ event: Event) -> String {
        let start = event.startsAt.formatted(date: .omitted, time: .shortened)
        let end = event.endsAt.formatted(date: .omitted, time: .shortened)
        return "\(start) – \(end)"
    }
}

// MARK: - the month's cells

/// One month's cells: whole weeks starting on the locale's first weekday, so
/// every row is seven days and the columns stay under the weekday header. The
/// leading and trailing cells belong to the neighbouring months and are drawn
/// muted.
struct MonthGrid: Identifiable {
    let month: Date         // start of the month
    let weeks: [[Date]]     // day-starts, including the neighbouring filler

    var id: Date { month }
    var title: String { month.formatted(.dateTime.month(.wide).year()) }

    func contains(_ day: Date) -> Bool {
        Calendar.current.isDate(day, equalTo: month, toGranularity: .month)
    }

    init(month: Date, calendar: Calendar = .current) {
        self.month = month
        guard let days = calendar.range(of: .day, in: .month, for: month) else {
            self.weeks = []
            return
        }
        // How many days of the previous month lead this one in, given where the
        // locale starts its week.
        let lead = (calendar.component(.weekday, from: month) - calendar.firstWeekday + 7) % 7
        let rows = Int((Double(lead + days.count) / 7).rounded(.up))
        let start = calendar.date(byAdding: .day, value: -lead, to: month) ?? month
        self.weeks = (0..<rows).map { row in
            (0..<7).compactMap { column in
                // Normalized to midnight: these are the keys events are grouped
                // by, and a DST transition can otherwise leave an added day an
                // hour off its own start.
                calendar.date(byAdding: .day, value: row * 7 + column, to: start)
                    .map(calendar.startOfDay(for:))
            }
        }
    }

    /// The months the grid covers: from the earlier of the oldest event's month
    /// and last month, through the later of the newest event's month and this
    /// one — so an empty calendar still shows last month and this one, and a
    /// calendar with history scrolls back over all of it.
    static func range(covering events: [Event], now: Date = Date()) -> [MonthGrid] {
        let calendar = Calendar.current
        let thisMonth = month(of: now)
        let lastMonth = calendar.date(byAdding: .month, value: -1, to: thisMonth) ?? thisMonth
        // The store holds events ascending by start, so the ends of the array
        // are the oldest and newest without sorting again.
        let oldest = events.first.map { month(of: $0.startsAt) } ?? thisMonth
        let newest = events.last.map { month(of: $0.startsAt) } ?? thisMonth

        var cursor = Swift.min(oldest, lastMonth)
        let end = Swift.max(newest, thisMonth)
        var out: [MonthGrid] = []
        while cursor <= end {
            out.append(MonthGrid(month: cursor))
            guard let next = calendar.date(byAdding: .month, value: 1, to: cursor) else { break }
            cursor = next
        }
        return out
    }

    static func month(of date: Date, calendar: Calendar = .current) -> Date {
        calendar.date(from: calendar.dateComponents([.year, .month], from: date)) ?? date
    }

    /// The locale's weekday letters, rotated so the first column is the
    /// locale's own first weekday — Sunday in the US, Monday in much of Europe.
    static var weekdaySymbols: [String] {
        let calendar = Calendar.current
        let symbols = calendar.veryShortWeekdaySymbols
        let offset = calendar.firstWeekday - 1
        guard offset > 0, offset < symbols.count else { return symbols }
        return Array(symbols[offset...] + symbols[..<offset])
    }
}
