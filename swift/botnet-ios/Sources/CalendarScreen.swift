// CalendarScreen.swift — every instance the server holds, grouped by day and
// led by what is still ahead. Bots write to the same calendar the user does,
// so each row also says who put it there; that attribution is the whole reason
// this is a service and not a local store.
//
// Read-only on the phone: no create, no edit, no delete, and a row navigates
// nowhere. Editing an instance means editing the MASTER event it expands from,
// which is a different object with a recurrence rule the phone has no editor
// for — a tap that silently changed a whole series would be worse than a tap
// that does nothing.

import SwiftUI

struct CalendarScreen: View {
    @EnvironmentObject var store: AppStore

    private var groups: PhoneEventGroups { PhoneEventGroups(store.instances) }

    var body: some View {
        Group {
            if store.instances.isEmpty { empty } else { list }
        }
        .background(Palette.chrome)
        .navigationTitle("Calendar")
        .navigationBarTitleDisplayMode(.inline)
        // The calendar is shared state a bot can change between visits, so
        // opening the screen re-reads it rather than trusting the launch fetch.
        // (refreshEvents re-reads the calendars and the instances too.)
        .task { await store.refreshEvents() }
    }

    private var list: some View {
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 0) {
                ForEach(Array(groups.upcoming.enumerated()), id: \.element.id) { index, day in
                    dayGroup(day).padding(.top, index == 0 ? 0 : Metric.dayGroupGap)
                }
                if !groups.past.isEmpty {
                    Text("Earlier")
                        .font(TypeScale.phoneDayHeader)
                        .foregroundStyle(Palette.secondaryText)
                        .padding(.top, groups.upcoming.isEmpty ? 0 : Metric.dayGroupGap)
                    ForEach(groups.past) { day in
                        dayGroup(day).padding(.top, Metric.dayGroupGap)
                    }
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, Metric.phoneHPad)
            .padding(.vertical, Metric.phoneVPad)
        }
        .refreshable { await store.refreshEvents() }
    }

    private func dayGroup(_ day: PhoneEventDay) -> some View {
        VStack(alignment: .leading, spacing: Metric.eventRowGap) {
            Text(day.label)
                .font(TypeScale.phoneDayHeader)
                .foregroundStyle(day.isPast ? Palette.secondaryText : Palette.primaryText)
                .padding(.bottom, Metric.phoneTightGap)
            ForEach(day.instances) { instance in
                EventRow(instance: instance,
                         creator: creator(of: instance),
                         calendarColor: calendarColor(of: instance))
            }
        }
    }

    private var empty: some View {
        ContentUnavailableView {
            Label("No events", systemImage: "calendar")
        } description: {
            Text("Bots write to this calendar too — ask one to put something on it.")
        }
    }

    /// The instance's calendar color, resolved against the live calendar list
    /// so a recolor propagates without a refetch. Nil — no dot at all — when it
    /// has no calendar (old server) or its calendar is gone; an unknown color
    /// *string* still dots, in the palette's neutral fallback.
    private func calendarColor(of instance: EventInstance) -> Color? {
        store.calendar(id: instance.calendarId).map { Palette.calendar($0.color) }
    }

    // Resolved against the live bot list at render time, never captured onto
    // the row, so a renamed bot renames here without a refetch.
    private func creator(of instance: EventInstance) -> PhoneEventCreator {
        guard !instance.isUserCreated else { return .user }
        let name = store.bots.first { $0.id == instance.createdBy }?.displayName
        return .bot(id: instance.createdBy, name: name ?? "a bot")
    }
}

/// Who put an event on the calendar.
enum PhoneEventCreator {
    case user
    /// A bot that may no longer be in the list, so the name is resolved, not
    /// assumed — a deleted bot's events stay on the calendar.
    case bot(id: String, name: String)
}

private struct EventRow: View {
    let instance: EventInstance
    let creator: PhoneEventCreator
    let calendarColor: Color?

    var body: some View {
        HStack(alignment: .top, spacing: Metric.phoneRowGap) {
            time
            VStack(alignment: .leading, spacing: Metric.phoneTightGap) {
                HStack(spacing: Metric.phoneTightGap) {
                    // The calendar's color, right where the eye already is. A
                    // dot rather than a tint: six tinted rows would turn the
                    // list into a heat map, and a dot reads identically on the
                    // light and dark grounds.
                    if let calendarColor {
                        Circle().fill(calendarColor)
                            .frame(width: Metric.calendarDot, height: Metric.calendarDot)
                    }
                    Text(instance.title)
                        .font(TypeScale.phoneRowTitle)
                        .foregroundStyle(Palette.primaryText)
                        .lineLimit(1)
                    // The firing affordances, right after the title where the
                    // row is read: repeat = this is one occurrence of a series,
                    // bolt = an automation fires while it is active.
                    if instance.recurring {
                        Image(systemName: "repeat")
                            .font(TypeScale.eventGlyph)
                            .foregroundStyle(Palette.secondaryText)
                    }
                    if instance.firesAutomation {
                        Image(systemName: "bolt.fill")
                            .font(TypeScale.eventGlyph)
                            .foregroundStyle(Palette.attention)
                    }
                }
                if instance.hasLocation {
                    Label(instance.location ?? "", systemImage: "mappin.and.ellipse")
                        .font(TypeScale.phoneRowMeta)
                        .foregroundStyle(Palette.secondaryText)
                        .lineLimit(1)
                }
            }
            Spacer(minLength: Metric.phoneTightGap)
            author
        }
        .padding(.vertical, Metric.phoneRowVPad)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    // A fixed column so every title on the day starts at the same x; the end
    // time is quieter because the start is what the eye scans for.
    private var time: some View {
        VStack(alignment: .trailing, spacing: 1) {
            Text(Self.clock(instance.startsAt))
                .foregroundStyle(Palette.primaryText)
            Text(Self.endClock(instance))
                .foregroundStyle(Palette.secondaryText)
        }
        .font(TypeScale.phoneRowMeta)
        .lineLimit(1)
        .frame(width: Metric.eventTimeWidth, alignment: .trailing)
    }

    private var author: some View {
        HStack(spacing: Metric.phoneTightGap) {
            switch creator {
            case .user:
                Image(systemName: "person.crop.circle")
                Text("you")
            case .bot(let id, let name):
                BotAvatar(botID: id, size: Metric.avatarSmall)
                Text(name)
            }
        }
        .font(TypeScale.phoneRowMeta)
        .foregroundStyle(Palette.secondaryText)
        .lineLimit(1)
    }

    private static func clock(_ date: Date) -> String {
        date.formatted(date: .omitted, time: .shortened)
    }

    // An overnight flight files under the day it leaves, so its end time would
    // otherwise read as twelve hours before its start. The day count says so.
    private static func endClock(_ instance: EventInstance) -> String {
        let end = Calendar.current.startOfDay(for: instance.endsAt)
        let days = Calendar.current.dateComponents([.day], from: instance.day, to: end).day ?? 0
        return days > 0 ? "\(clock(instance.endsAt)) +\(days)" : clock(instance.endsAt)
    }
}

// MARK: - grouping

// The Mac twin is `EventGroups`/`EventDay` in swift/botnet/Sources/CalendarView.swift,
// and the rules below are deliberately identical to it. It is not shared because
// it lives inside a file the phone cannot compile — CalendarView.swift drags in
// ServiceKind, the month grid and the event sheets — and lifting it out is a
// reshape of a file another agent is editing. Two copies of a grouping rule is a
// real cost; keep them in step, and fold this one back the moment the Mac's
// grouping moves to a file of its own.

/// The event list as it reads: the days still ahead in order, then the days
/// already gone, most recent first. Leading with last month would make the
/// screen useless; dropping it would make it a lie.
struct PhoneEventGroups {
    var upcoming: [PhoneEventDay] = []
    var past: [PhoneEventDay] = []

    init(_ instances: [EventInstance], now: Date = Date()) {
        let today = Calendar.current.startOfDay(for: now)
        let byDay = Dictionary(grouping: instances, by: \.day)
        let days = byDay.map { date, instances in
            PhoneEventDay(date: date, instances: instances.sorted {
                ($0.startsAt, $0.id) < ($1.startsAt, $1.id)
            }, today: today)
        }
        upcoming = days.filter { !$0.isPast }.sorted { $0.date < $1.date }
        past = days.filter(\.isPast).sorted { $0.date > $1.date }
    }
}

struct PhoneEventDay: Identifiable {
    let date: Date
    let instances: [EventInstance]
    let isPast: Bool
    let label: String

    var id: Date { date }

    init(date: Date, instances: [EventInstance], today: Date) {
        self.date = date
        self.instances = instances
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
