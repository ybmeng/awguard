// FactLead.swift — what the Add Fact sheet's lead row SAYS on BOTH apps, kept out of the
// view so it can be proven without one (dev/lead-check.sh).
//
// The rule it encodes is a server rule, not a display choice. Creating a dated
// fact with leadDays == 0 does not produce a zero-day due_soon window: 0 is the
// server's UNSET sentinel, and go/botnet/projects.go resolves the fact's
// EffectiveLeadDays to the project's whenever its own is 0 (leadFor). So a row
// reading "Lead 0 days" would name a window that never applies, and the user
// would be told 0 and given 180.
//
// The server stores the authored 0 and derives the answer alongside it; it does
// NOT rewrite the stored value, so `ProjectFact.effectiveLeadDays` is what any
// reader renders. That split is why this file takes the project rather than the
// fact: at create time there is no fact yet to carry a derived field.
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
    ///
    /// The wording itself comes from `ProjectInheritance.leadStepperText`,
    /// which the project sheets' "Default lead" row also calls: the same
    /// inherit-at-0 rule applies at both levels, and two copies of the phrasing
    /// would drift the first time either was reworded. What differs here is the
    /// number a 0 resolves to — a fact inherits its PROJECT's lead, where a
    /// project inherits its ancestor's — so that is what this passes in.
    static func label(draft: Int, project: Project) -> String {
        ProjectInheritance.leadStepperText("Lead", draft: draft,
                                           whenInherited: inherited(of: project))
    }

    /// What a 0 draft resolves to: the lead the server derived for this project,
    /// or the global fallback on a botnetd that derives none.
    static func inherited(of project: Project) -> Int {
        project.hasEffectiveLead ? project.effectiveLeadDays : Project.globalDefaultLeadDays
    }

    /// Where the sheet's stepper starts. 0, deliberately: 0 is the inherit
    /// sentinel on both create and patch, so an untouched sheet sends 0 and
    /// the fact keeps following its project's lead; the row reads
    /// "Lead: inherited (N d)" so the user still sees the number. Seeding the
    /// number itself would author it, and the fact would stop inheriting.
    static func initialDraft(for project: Project) -> Int { 0 }

    /// What a FACT ROW's lead reads in the pane — a different reading from the
    /// stepper above, and deliberately so.
    ///
    /// The number is always `effectiveLeadDays`, because that is the window the
    /// server actually computes due_soon from. What the row adds is WHERE it
    /// came from: "180d lead" and "180d lead (project)" are the same number in
    /// two different states, and only the second follows the project when its
    /// default is changed. A reader deciding whether to edit the fact or the
    /// project needs that told, and it is invisible from the number alone.
    ///
    /// Nil when there is no window at all — an undated kind, or a botnetd that
    /// derives none — so the row omits the text rather than printing "0d lead".
    /// A server predating the derived key sends no `effectiveLeadDays`, which
    /// decodes as the authored `leadDays`; such a fact reads exactly as it did
    /// before the key existed, never "(project)", since that server cannot
    /// perform the inheritance the suffix would be claiming.
    static func rowLabel(for fact: ProjectFact) -> String? {
        guard fact.effectiveLeadDays > 0 else { return nil }
        guard fact.leadDays == 0 else { return "\(fact.effectiveLeadDays)d lead" }
        return "\(fact.effectiveLeadDays)d lead (project)"
    }
}
