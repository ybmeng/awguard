// MonthGridView.swift — the Calendar service's second reading of the same
// events. The list answers "what is next"; the grid answers "what does this
// month look like", which is the question a list can't answer at a glance.
//
// The grid owns no data and no sheet: it hands an EventTarget back to
// CalendarView, which presents the one editor both views share.

import SwiftUI

struct MonthGridView: View {
    @EnvironmentObject var store: AppStore
    /// The instances to draw — CalendarView passes the same filtered array the
    /// list renders, so the active chip means one thing in both readings.
    let instances: [EventInstance]
    /// Called with the tapped occurrence; CalendarView resolves it to the
    /// master event, which is what the editor opens.
    let openInstance: (EventInstance) -> Void
    /// Called with the day the empty area of a cell was clicked on.
    let createOn: (Date) -> Void

    private var months: [MonthGrid] { MonthGrid.range(covering: instances) }
    private var byDay: [Date: [EventInstance]] { Dictionary(grouping: instances, by: \.day) }

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

    /// Same resolution rule as the list rows: a dot only when the calendar
    /// resolves, so an old server's chips render exactly as before.
    private func calendarColor(of instance: EventInstance) -> Color? {
        store.calendar(id: instance.calendarId).map { Palette.calendar($0.color) }
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
                            // A trailing cell's instances belong to the next
                            // month's section; drawing them twice would make
                            // the same occurrence look like two.
                            instances: inMonth ? (byDay[day] ?? []) : [],
                            color: calendarColor(of:),
                            openInstance: openInstance,
                            createOn: createOn
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
    let instances: [EventInstance]
    let color: (EventInstance) -> Color?
    let openInstance: (EventInstance) -> Void
    let createOn: (Date) -> Void

    @State private var hovering = false

    /// A cell shows this many chips and then counts the rest. A fourth chip
    /// does not fit the cell's height, and a cell that scrolled internally
    /// would fight the month list scrolling around it.
    private static let maxChips = 3

    private var visible: [EventInstance] { Array(instances.prefix(Self.maxChips)) }
    private var overflow: Int { instances.count - visible.count }
    // Gated on inMonth so today is marked once, in its own month's section,
    // not again on a neighbouring section's muted filler cell.
    private var isToday: Bool { inMonth && Calendar.current.isDateInToday(day) }

    var body: some View {
        ZStack(alignment: .topLeading) {
            // The empty area of the cell is the "add an event here" target. It
            // sits under the labels, which opt out of hit testing so the click
            // reaches it; the chips keep theirs and open their own event.
            Button { createOn(day) } label: {
                Rectangle().fill(background).contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .disabled(!inMonth)
            .help(inMonth ? "Add an event on \(day.formatted(date: .abbreviated, time: .omitted))" : "")

            VStack(alignment: .leading, spacing: 1) {
                dayNumber
                ForEach(visible) { instance in
                    EventChip(instance: instance, color: color(instance)) {
                        openInstance(instance)
                    }
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
    let instance: EventInstance
    let color: Color?
    let open: () -> Void

    var body: some View {
        Button(action: open) {
            HStack(spacing: 3) {
                // A dot, not a tint: at three chips per hundred-point cell a
                // tinted background would leave the title unreadable in dark
                // mode, and chipDot is small enough not to cost title width.
                if let color {
                    Circle().fill(color)
                        .frame(width: Metric.chipDot, height: Metric.chipDot)
                }
                Text(Self.clock(instance.startsAt))
                    .foregroundStyle(Palette.secondaryText)
                Text(instance.title)
                    .foregroundStyle(Palette.primaryText)
                    .lineLimit(1)
                Spacer(minLength: 0)
                // Same marks as the list rows, at chip scale — this is where a
                // recurring series is actually SEEN repeating across days.
                if instance.recurring {
                    Image(systemName: "repeat")
                        .font(TypeScale.chipGlyph)
                        .foregroundStyle(Palette.secondaryText)
                }
                if instance.firesAutomation {
                    Image(systemName: "bolt.fill")
                        .font(TypeScale.chipGlyph)
                        .foregroundStyle(Palette.attention)
                }
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
        .help(instance.firesAutomation
              ? "\(instance.title) — \(Self.range(instance)) — fires \(instance.automation ?? "")"
              : "\(instance.title) — \(Self.range(instance))")
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

    private static func range(_ instance: EventInstance) -> String {
        let start = instance.startsAt.formatted(date: .omitted, time: .shortened)
        let end = instance.endsAt.formatted(date: .omitted, time: .shortened)
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

    /// The months the grid covers: from the earlier of the oldest instance's
    /// month and last month, through the later of the newest instance's month
    /// and this one — so an empty calendar still shows last month and this one,
    /// and a calendar with history scrolls back over all of it (bounded by the
    /// store's instance window on a current server).
    static func range(covering instances: [EventInstance], now: Date = Date()) -> [MonthGrid] {
        let calendar = Calendar.current
        let thisMonth = month(of: now)
        let lastMonth = calendar.date(byAdding: .month, value: -1, to: thisMonth) ?? thisMonth
        // The store holds instances ascending by start, so the ends of the
        // array are the oldest and newest without sorting again.
        let oldest = instances.first.map { month(of: $0.startsAt) } ?? thisMonth
        let newest = instances.last.map { month(of: $0.startsAt) } ?? thisMonth

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
