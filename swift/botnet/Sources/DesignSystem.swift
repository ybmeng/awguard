// DesignSystem.swift — the visual vocabulary the whole app draws from: colors,
// layout metrics, and the per-bot avatar. Views must not hardcode a color or a
// spacing number; every value lives here so the look can be retuned in one file.

import SwiftUI
#if canImport(AppKit)
import AppKit
#else
import UIKit
#endif

// MARK: - Palette

// Each color adapts to the system appearance. The light values are the target
// look: white chrome, a warm gray bot bubble, a near-black user bubble. Dark
// mode inverts the bubble pair rather than tinting it, which keeps the same
// "one speaker is solid, one is quiet" contrast that makes the transcript read.
enum Palette {
    static let chrome = dynamic(light: 0xFFFFFF, dark: 0x1C1C1E)
    static let botBubble = dynamic(light: 0xF1F0EE, dark: 0x2C2C2E)
    static let botBubbleText = dynamic(light: 0x121212, dark: 0xF2F2F2)
    static let userBubble = dynamic(light: 0x0B0B0C, dark: 0xEDEDED)
    static let userBubbleText = dynamic(light: 0xFFFFFF, dark: 0x101010)
    static let rowSelected = dynamic(light: 0xE8E8E6, dark: 0x313134)
    static let rowHover = dynamic(light: 0xF3F3F1, dark: 0x28282A)
    static let fieldFill = dynamic(light: 0xF0F0EE, dark: 0x2A2A2C)
    static let fieldStroke = dynamic(light: 0xE0E0DD, dark: 0x3A3A3C)
    static let hairline = dynamic(light: 0xE4E4E1, dark: 0x38383A)
    static let primaryText = dynamic(light: 0x111111, dark: 0xF2F2F2)
    static let secondaryText = dynamic(light: 0x8B8B90, dark: 0x98989E)
    /// A third step down, for a qualifier that must not compete with the thing
    /// it qualifies — the project header's "via <parent>" beside its owner's
    /// name. Set apart from secondaryText enough to read as a lower tier at
    /// rowMeta size without disappearing on either ground.
    static let tertiaryText = dynamic(light: 0xAEAEB2, dark: 0x76767C)
    static let attention = dynamic(light: 0xE1830B, dark: 0xF0A03C)

    // The six calendar colors, named for the server's color enum. Each pair is
    // tuned to read as a small dot on both the light and dark chrome — the dark
    // variants are lifted, not just reused, because the light values sink into
    // the dark ground at dot size.
    static let calendarBlue = dynamic(light: 0x3574E0, dark: 0x6BA0F5)
    static let calendarGreen = dynamic(light: 0x3D9B4E, dark: 0x62C273)
    static let calendarOrange = dynamic(light: 0xE07A16, dark: 0xF0A04C)
    static let calendarPurple = dynamic(light: 0x8E5BD9, dark: 0xB48AF0)
    static let calendarRed = dynamic(light: 0xD9463C, dark: 0xF07A72)
    static let calendarTeal = dynamic(light: 0x0F9490, dark: 0x45C4C0)

    /// The wire color string to its token. A value this build doesn't know —
    /// a newer server's seventh color — draws as the quiet neutral rather than
    /// failing or shouting; secondaryText is already that gray on both grounds.
    static func calendar(_ wire: String) -> Color {
        switch wire {
        case "blue": return calendarBlue
        case "green": return calendarGreen
        case "orange": return calendarOrange
        case "purple": return calendarPurple
        case "red": return calendarRed
        case "teal": return calendarTeal
        default: return secondaryText
        }
    }

    /// An automation's freshness to its sidebar-dot color. Quiet by default:
    /// only a healthy schedule is green and only trouble is loud — pending,
    /// never and unscheduled are states, not problems, so they share the
    /// neutral gray. An unknown future value falls back the same way.
    static func freshness(_ wire: String) -> Color {
        switch wire {
        case "ok": return calendarGreen
        case "stale": return attention
        case "failed": return calendarRed
        default: return secondaryText // pending, never, unscheduled, future values
        }
    }

    // A project's health. Its own four tokens rather than borrowed calendar
    // ones: health is the app's only scale where a HUMAN gate (blocked) has to
    // read as different in kind from a clock problem (overdue, due soon), so
    // these have to be retunable without dragging the calendar's colors along.
    // Same lift-for-dark rule as the calendar pair — a dot this small sinks
    // into the dark ground otherwise.
    static let healthOverdue = dynamic(light: 0xD23C31, dark: 0xF07068)
    static let healthBlocked = dynamic(light: 0x8E5BD9, dark: 0xB48AF0)
    static let healthDueSoon = dynamic(light: 0xE1830B, dark: 0xF0A03C)
    static let healthOK = dynamic(light: 0x3D9B4E, dark: 0x62C273)

    /// A project's health string to its dot color. Precedence order, loudest
    /// first. "unknown" (zero facts) is a state, not a problem, so it shares
    /// the neutral gray with any value this build doesn't know — the same
    /// stance freshness(_:) takes.
    static func health(_ wire: String) -> Color {
        switch wire {
        case "overdue": return healthOverdue
        case "blocked": return healthBlocked
        case "due_soon": return healthDueSoon
        case "ok": return healthOK
        default: return secondaryText // unknown, future values
        }
    }

    /// A project's severity to its dot color: S0 act now, S1 should be doing,
    /// S2 tracked but not pressing. Severity is a COARSENING of health, so it
    /// reuses health's tokens rather than opening a second palette that could
    /// drift out of step with the first — and S2 is deliberately the quiet gray
    /// even for a healthy project, because the scale answers "does this need me
    /// now", not "is this fine". An unknown value falls back the same way the
    /// health and freshness maps do.
    static func severity(_ wire: String) -> Color {
        switch wire {
        case "S0": return healthOverdue
        case "S1": return healthDueSoon
        default: return secondaryText // S2, future values
        }
    }

    /// The dot beside a project's name. Severity is the scale the tree reads
    /// by, but a botnetd that predates it sends none, and graying every project
    /// on an older server would be a silent lie — so health is the fallback,
    /// in one place rather than at each call site.
    static func projectDot(severity: String, health: String) -> Color {
        severity.isEmpty ? self.health(health) : self.severity(severity)
    }

    /// A run's status to its label color — the same temperature scale as the
    /// freshness dot: green for a clean envelope, orange for degraded and
    /// needs_human (the run spoke, but wants eyes), red for failed and for
    /// error (no envelope at all), gray for the in-flight states.
    static func runStatus(_ wire: String) -> Color {
        switch wire {
        case "ok": return calendarGreen
        case "degraded", "needs_human": return attention
        case "failed", "error": return calendarRed
        default: return secondaryText // queued, running, future values
        }
    }

    // The avatar colors. Index is chosen by hashing the bot id, so a bot keeps
    // the same face for its whole life without storing anything server-side.
    static let avatarColors: [Color] = [
        solid(0xE8524A), solid(0xF08A2B), solid(0x3FBFA8), solid(0xF06BA8),
        solid(0x9B7BE8), solid(0x4A8FE8), solid(0xEFC03C), solid(0x5BB85B),
    ]

    // The tokens above are platform-neutral; only the bridge from a hex pair to
    // an appearance-aware Color needs the host toolkit, so that bridge is the
    // one thing that forks. Both sides resolve the SAME hexes — the iOS app
    // shares this file, and a second palette would drift from the Mac's.
    #if canImport(AppKit)
    private static func dynamic(light: UInt32, dark: UInt32) -> Color {
        Color(nsColor: NSColor(name: nil) { appearance in
            let isDark = appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
            return nsColor(isDark ? dark : light)
        })
    }

    private static func solid(_ hex: UInt32) -> Color { Color(nsColor: nsColor(hex)) }

    private static func nsColor(_ hex: UInt32) -> NSColor {
        NSColor(
            srgbRed: Double((hex >> 16) & 0xFF) / 255,
            green: Double((hex >> 8) & 0xFF) / 255,
            blue: Double(hex & 0xFF) / 255,
            alpha: 1
        )
    }
    #else
    private static func dynamic(light: UInt32, dark: UInt32) -> Color {
        Color(uiColor: UIColor { traits in
            uiColor(traits.userInterfaceStyle == .dark ? dark : light)
        })
    }

    private static func solid(_ hex: UInt32) -> Color { Color(uiColor: uiColor(hex)) }

    private static func uiColor(_ hex: UInt32) -> UIColor {
        UIColor(
            red: Double((hex >> 16) & 0xFF) / 255,
            green: Double((hex >> 8) & 0xFF) / 255,
            blue: Double(hex & 0xFF) / 255,
            alpha: 1
        )
    }
    #endif
}

// MARK: - Metrics

enum Metric {
    static let sidebarWidth: CGFloat = 292
    static let sidebarGutter: CGFloat = 10
    static let rowRadius: CGFloat = 9
    static let rowVPad: CGFloat = 7

    static let avatarSmall: CGFloat = 20
    static let avatarRow: CGFloat = 30

    /// How far a section's rows sit in from its chevron header — what makes
    /// the sidebar read as an explorer tree rather than three flat lists.
    static let sidebarIndent: CGFloat = 12
    /// The fixed chevron column on a section header, so the three section
    /// labels start at one x whichever way their carets point.
    static let sectionChevronWidth: CGFloat = 12
    /// An automation's freshness dot in the sidebar; same size family as
    /// calendarDot, separate because the two can retune apart.
    static let freshnessDot: CGFloat = 7

    /// The automation pane's content column stops growing here, same reason
    /// as calendarListWidth: a run row's trailing fields must stay near the
    /// name, not a window's width away.
    static let automationListWidth: CGFloat = 680
    /// The project pane's facts column, capped for the same reason: a fact's
    /// due text and blocker chip belong beside its title, not a window away.
    static let projectListWidth: CGFloat = 680
    /// A project's health dot in the sidebar and in the pane's badge. Same size
    /// family as freshnessDot, separate so the two can retune apart.
    static let healthDot: CGFloat = 7
    /// The owner's avatar in the project pane's header. Smaller than
    /// avatarSmall: it sits inline with rowMeta text beside the health badge,
    /// where a sidebar-sized blob would outweigh the project's own name.
    static let ownerAvatar: CGFloat = 16
    /// How far one level of the project tree indents its rows. Smaller than
    /// sidebarIndent (which sets a whole section in from its header): these
    /// steps stack, and a section three projects deep must not run out of row.
    static let projectTreeIndent: CGFloat = 11
    /// The disclosure caret's fixed column on a project row. A leaf reserves
    /// the same width and draws nothing, so sibling names line up whether or
    /// not they have children.
    static let projectDisclosureWidth: CGFloat = 12
    /// A child row in the pane's sub-projects strip. Its own token because the
    /// strip is a list of links, not the facts list, and the two retune apart.
    static let childStripRowGap: CGFloat = 1
    /// A fact row's leading kind glyph, fixed so every title starts at one x
    /// whichever glyph the kind uses.
    static let factGlyphWidth: CGFloat = 16
    /// The done checkbox on a completable fact row, sized to sit beside
    /// rowTitle text without becoming the loudest thing in the row.
    static let factToggle: CGFloat = 14
    /// A disclosed run's stderr tail scrolls inside this rather than growing
    /// the row by up to 8KB of text.
    static let stderrMaxHeight: CGFloat = 160

    static let bubbleRadius: CGFloat = 17
    static let bubbleHPad: CGFloat = 14
    static let bubbleVPad: CGFloat = 9
    /// Gap between bubbles inside one speaker's turn.
    static let bubbleGap: CGFloat = 4
    /// Gap where the speaker changes. The contrast between these two numbers is
    /// what groups a multi-paragraph answer into a single visual block.
    static let turnGap: CGFloat = 26
    /// A bubble never grows past this share of the transcript width, so long
    /// answers keep a readable measure instead of spanning the window.
    static let bubbleWidthFraction: CGFloat = 0.50

    static let transcriptHPad: CGFloat = 22
    static let transcriptVPad: CGFloat = 20

    static let composerRadius: CGFloat = 22
    static let composerMinHeight: CGFloat = 44
    static let composerPad: CGFloat = 12

    static let headerHeight: CGFloat = 44

    static let inspectorPad: CGFloat = 12
    /// A panel section grows with its content up to this, then scrolls inside.
    static let inspectorSectionMaxHeight: CGFloat = 300
    /// Inspector sections draw as rounded cards; the radius, the gap between
    /// cards, and the inset from the inspector edges are one treatment, so the
    /// card look retunes here rather than per section.
    static let inspectorCardRadius: CGFloat = 9
    static let inspectorCardSpacing: CGFloat = 8
    static let inspectorCardInset: CGFloat = 12

    /// Fixed width for a tool-call row's leading icon, so the summaries line up
    /// down the list even though each tool's glyph is a different width.
    static let toolIconWidth: CGFloat = 15

    /// The calendar's leading time column. Wide enough for "12:00 AM +1" at
    /// rowMeta, so every title in the list starts at the same x.
    static let eventTimeWidth: CGFloat = 76
    /// The event list stops growing here for the same reason a bubble does: the
    /// author sitting at the row's trailing edge has to stay next to the title,
    /// not a window's width away from it.
    static let calendarListWidth: CGFloat = 680
    /// Between two events on the same day.
    static let eventRowGap: CGFloat = 2
    /// Between one day's group and the next — large enough that the day headers
    /// read as the structure of the list.
    static let dayGroupGap: CGFloat = 18

    /// A month-grid day cell: tall enough for the date plus three chips and an
    /// overflow line, which is what the cell is allowed to show.
    static let monthCellHeight: CGFloat = 96
    /// The gap between day cells. The grid's own background shows through it,
    /// which is what draws the rules — there are no separate divider views.
    static let monthCellGap: CGFloat = 1
    /// A day's event chip.
    static let chipRadius: CGFloat = 4
    static let chipHPad: CGFloat = 4
    static let chipVPad: CGFloat = 2
    /// The circle behind today's date number.
    static let todayMarker: CGFloat = 19

    /// A calendar's color dot: on a list row and a filter chip.
    static let calendarDot: CGFloat = 7
    /// The header's type-to-filter field. Fixed, not flexible: the header's
    /// controls keep their place as the window resizes.
    static let calendarSearchWidth: CGFloat = 200
    /// The same dot inside a month-grid chip, where three chips share a cell
    /// barely a hundred points wide and the dot must not eat title width.
    static let chipDot: CGFloat = 5
}

enum TypeScale {
    static let message = Font.system(size: 13.5)
    static let rowTitle = Font.system(size: 13, weight: .semibold)
    static let rowPreview = Font.system(size: 12)
    static let rowMeta = Font.system(size: 11.5)
    static let headerTitle = Font.system(size: 13, weight: .semibold)
    static let composer = Font.system(size: 13.5)
    /// Per-message metadata under a bubble: ids and status, deliberately small
    /// and monospaced so an id is readable and never competes with the message.
    static let messageMeta = Font.system(size: 10, design: .monospaced)
    /// Verbatim machine text shown as a block (a tool's parameters schema):
    /// monospaced, one step up from messageMeta so indentation stays legible.
    static let codeBlock = Font.system(size: 11, design: .monospaced)
    /// The rotating disclosure chevron on an inspector section header.
    static let sectionChevron = Font.system(size: 9, weight: .semibold)
    /// A group label over a short list ("Services") — quiet enough that it
    /// organizes the rows without competing with them.
    static let sectionLabel = Font.system(size: 10.5, weight: .semibold)
    /// A service row's leading glyph, sized to sit beside rowTitle text.
    static let serviceIcon = Font.system(size: 14)
    /// A day heading in the calendar list.
    static let dayHeader = Font.system(size: 12, weight: .semibold)
    /// The month name over a grid section.
    static let monthHeader = Font.system(size: 13, weight: .semibold)
    /// A day number in a grid cell.
    static let dayNumber = Font.system(size: 11, weight: .medium)
    /// The weekday letters over the grid, and a cell's "+2 more" line.
    static let gridMeta = Font.system(size: 10)
    /// An event chip inside a day cell — the smallest readable text in the app.
    static let chip = Font.system(size: 10.5)
    /// The repeat/bolt marks on an event row, a calendar chip, and a manage
    /// row — sized to sit beside rowTitle/rowMeta text without competing.
    static let eventGlyph = Font.system(size: 10, weight: .medium)
    /// The same marks inside a month-grid chip, where three chips share a cell
    /// barely a hundred points wide.
    static let chipGlyph = Font.system(size: 8, weight: .medium)
}

// MARK: - Avatar

/// The soft blob every bot is identified by. Silhouette and color are both
/// derived from the bot id, so the same bot always draws the same face and no
/// two adjacent bots in the sidebar tend to collide.
struct BotAvatar: View {
    let botID: String
    var size: CGFloat = Metric.avatarRow

    // Color and silhouette use independently seeded hashes. Sharing one hash
    // and taking `% count` off it collapsed the palette onto a few colors.
    private var shapeSeed: UInt64 { Self.hash(botID, basis: 0xcbf29ce484222325) }
    private var colorSeed: UInt64 { Self.hash(botID, basis: 0x84222325cbf29ce4) }

    // FNV-1a alone leaves the low bits correlated for ids that share a long
    // prefix, which is exactly our case and which collapsed `% colorCount`
    // onto three colors. The splitmix64 finalizer spreads them.
    private static func hash(_ s: String, basis: UInt64) -> UInt64 {
        var h = basis
        for byte in s.utf8 {
            h = (h ^ UInt64(byte)) &* 0x100000001b3
        }
        h = (h ^ (h >> 30)) &* 0xbf58476d1ce4e5b9
        h = (h ^ (h >> 27)) &* 0x94d049bb133111eb
        return h ^ (h >> 31)
    }

    var body: some View {
        let color = Palette.avatarColors[Int(colorSeed % UInt64(Palette.avatarColors.count))]
        BlobShape(seed: shapeSeed)
            .fill(color)
            .overlay(Eyes(size: size))
            .frame(width: size, height: size)
    }
}

/// A closed loop through five points whose radius and angle both wobble with
/// the seed. Drawn with quadratic curves between edge midpoints, which keeps
/// the outline smooth instead of polygonal. The wobble has to be this wide or
/// every bot renders as the same circle.
private struct BlobShape: Shape {
    let seed: UInt64

    func path(in rect: CGRect) -> Path {
        let center = CGPoint(x: rect.midX, y: rect.midY)
        let radius = min(rect.width, rect.height) / 2
        var state = seed | 1

        func unit() -> Double {
            state = state &* 6364136223846793005 &+ 1442695040888963407
            return Double((state >> 33) % 1000) / 1000
        }

        let count = 5
        let step: Double = 1 / Double(count)
        let points: [CGPoint] = (0..<count).map { i in
            let wobble: Double = 0.74 + 0.26 * unit()
            let jitter: Double = 0.28 * step * (unit() - 0.5)
            let angle: Double = (Double(i) * step + jitter) * 2 * Double.pi
            let dx: Double = radius * wobble * cos(angle)
            let dy: Double = radius * wobble * sin(angle)
            return CGPoint(x: center.x + dx, y: center.y + dy)
        }

        func midpoint(_ a: CGPoint, _ b: CGPoint) -> CGPoint {
            CGPoint(x: (a.x + b.x) / 2, y: (a.y + b.y) / 2)
        }

        var path = Path()
        path.move(to: midpoint(points[points.count - 1], points[0]))
        for i in 0..<points.count {
            let control = points[i]
            let end = midpoint(points[i], points[(i + 1) % points.count])
            path.addQuadCurve(to: end, control: control)
        }
        path.closeSubpath()
        return path
    }
}

private struct Eyes: View {
    let size: CGFloat

    var body: some View {
        HStack(spacing: size * 0.20) {
            eye
            eye
        }
        .offset(y: -size * 0.04)
    }

    private var eye: some View {
        Capsule()
            .fill(Color.black.opacity(0.82))
            .frame(width: size * 0.10, height: size * 0.17)
    }
}
