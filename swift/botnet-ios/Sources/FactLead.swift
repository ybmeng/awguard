// FactLead.swift — what the Add Fact sheet's lead row SAYS, kept out of the
// view so it can be proven without one (dev/lead-check.sh).
//
// The rule it encodes is a server rule, not a display choice. Creating a dated
// fact with leadDays == 0 does not store a zero-day due_soon window:
// createFact in go/botnet/projects.go substitutes the project's
// EffectiveLeadDays. So a row reading "Lead 0 days" would name a value the
// server will not keep, and the user would be told 0 and given 180.
//
// Foundation only, deliberately: this compiles for macOS as well as iOS, which
// is what lets the scratch harness assert the exact strings against the real
// Project type instead of eyeballing a screenshot.

import Foundation

enum FactLead {
    /// The lead row's label for a draft value.
    ///
    /// A draft of 0 means "take the project's", so the row names that number.
    /// Any other draft is the window that will actually be stored.
    static func label(draft: Int, project: Project) -> String {
        guard draft > 0 else { return "Lead: inherited (\(inherited(of: project)) d)" }
        return "Lead \(draft) \(draft == 1 ? "day" : "days")"
    }

    /// What a 0 draft resolves to: the lead the server derived for this project,
    /// or the global fallback on a botnetd that derives none.
    static func inherited(of project: Project) -> Int {
        project.hasEffectiveLead ? project.effectiveLeadDays : Project.globalDefaultLeadDays
    }

    /// Where the sheet's stepper starts, so the phone opens on the window the
    /// server would apply rather than on a number of its own.
    static func initialDraft(for project: Project) -> Int { inherited(of: project) }
}
