// MobileDesign.swift — the phone's additions to the shared design system.
//
// Palette is shared unchanged: a color that reads on a Mac reads on a phone.
// Sizes are not. The Mac tokens are tuned for a pointer at arm's length in a
// 1400pt window; a thumb on a 393pt screen needs bigger text, wider bubbles and
// 44pt targets. Every value the phone needs differently lives here, extending
// the same Metric/TypeScale namespaces, so an iOS view still never writes a
// number inline.

import SwiftUI

extension Metric {
    /// The Mac caps a bubble at half the transcript so a long answer keeps a
    /// readable measure across a wide window. A phone is already narrow: half
    /// of 393pt is a column three words wide, so the phone caps far later and
    /// leaves only enough gutter to tell the two speakers apart.
    static let phoneBubbleWidthFraction: CGFloat = 0.82

    static let phoneHPad: CGFloat = 16
    static let phoneVPad: CGFloat = 14
    /// The minimum comfortable touch target, per the platform's own guidance.
    /// Every tappable glyph on a screen sits in a frame at least this big.
    static let phoneTapTarget: CGFloat = 44
    static let phoneAvatar: CGFloat = 38
    /// The unread pip on a bot row.
    static let phoneUnreadDot: CGFloat = 8
    /// A row's own vertical padding — the Mac's 7pt reads as a cramped list on
    /// a phone, where the row IS the target.
    static let phoneRowVPad: CGFloat = 9
    /// The composer's send/attach circles.
    static let phoneComposerCircle: CGFloat = 32
    /// Across a row: a glyph or dot to the text beside it. The Mac writes 8
    /// inline beside 13pt text; the phone's rows carry 16pt titles, where the
    /// same gap reads as the two things touching.
    static let phoneRowGap: CGFloat = 10
    /// Down a row: a title over its preview, an RRULE under its fact. Tighter
    /// than phoneRowGap on purpose — the two gaps are what say which lines
    /// belong to one row.
    static let phoneTightGap: CGFloat = 4
    /// A fact's done glyph. The Mac's factToggle (14) sits beside 13pt row
    /// text; beside a 16pt phone title it reads as a speck, and this is the
    /// row's one action. Its hit frame is phoneTapTarget regardless.
    static let phoneFactToggle: CGFloat = 20
}

extension TypeScale {
    static let phoneMessage = Font.system(size: 16)
    static let phoneRowTitle = Font.system(size: 16, weight: .semibold)
    static let phoneRowPreview = Font.system(size: 14)
    static let phoneRowMeta = Font.system(size: 12.5)
    static let phoneComposer = Font.system(size: 16)
    /// A day heading in the calendar, and the "Facts" style label — one step up
    /// from the Mac's because it also has to survive being read one-handed.
    static let phoneDayHeader = Font.system(size: 14, weight: .semibold)
    /// Per-message metadata under a bubble: the id and the status.
    static let phoneMessageMeta = Font.system(size: 11, design: .monospaced)
    /// Verbatim machine text (an RRULE on a fact).
    static let phoneCodeBlock = Font.system(size: 12, design: .monospaced)
}
