// Store.swift — thin client over the Go botnet server. Holds NO durable state:
// every read and every mutation is an HTTP call to botnetd, which owns the
// bots, the messages, and the OpenRouter calls. This object only caches the
// server's responses for the views to render.

import Foundation

@MainActor
final class AppStore: ObservableObject {
    @Published private(set) var bots: [Bot] = []
    @Published private(set) var conversations: [String: [Message]] = [:] // botId → messages
    @Published private(set) var segments: [String: [Segment]] = [:]      // botId → chain
    @Published private(set) var models: [ModelOption] = ModelOption.roster
    // nil until fetched, and stays nil on a server without the route, so the
    // inspector simply omits its Tools section on an older botnetd.
    @Published private(set) var tools: [ToolDefinition]?
    // The calendar service's events, ascending by start as the server sorts
    // them. Empty on a botnetd without the routes, which reads as an empty
    // calendar rather than an error the user can do nothing about.
    @Published private(set) var events: [Event] = []
    // What the calendar pane renders: expanded instances over the fetch window,
    // ascending by start as the server sorts them. On a botnetd without
    // /v1/instances these are the events above mapped one-to-one, so the pane
    // reads exactly as it did before instances existed.
    @Published private(set) var instances: [EventInstance] = []
    // The named calendars, ascending by createdAt as the server sorts them.
    @Published private(set) var calendars: [EventCalendar] = []
    // True once /v1/calendars has 404'd: this botnetd predates multiple
    // calendars, so the views drop the calendar chrome (chip row, picker,
    // manage button) and the pane renders exactly as it did before them.
    @Published private(set) var calendarsUnavailable = false
    // The automations the bridge exposes, in the service's own order. Empty
    // both before the first fetch and on a server without the routes; the
    // sticky-ish flag below is what actually hides the nav section.
    @Published private(set) var automations: [Automation] = []
    // True once GET /v1/automations has 404'd: the bridge is unmounted (old
    // server, standalone botnetd) and the whole nav section is absent. Cleared
    // on a later success, so a restart onto a mounted server mid-run brings
    // the section back on the next refresh.
    @Published private(set) var automationsUnavailable = false
    // Full run rows fetched for inline disclosure, by run id. Cached so
    // reopening a row doesn't refetch a finished run (finished runs are
    // immutable); an unfinished run's row is refetched by the poll loop.
    @Published private(set) var runDetails: [String: RunDetail] = [:]
    // The projects, in the server's own order (health precedence, then nextDue,
    // then name). Empty both before the first fetch and on a server without the
    // routes; the flag below is what actually hides the nav section.
    @Published private(set) var projects: [Project] = []
    // True once GET /v1/projects has 404'd: this botnetd predates projects and
    // the whole nav section is absent, exactly the automations precedent.
    // Cleared on a later success, so a restart onto a current server mid-run
    // brings the section back on the next refresh.
    @Published private(set) var projectsUnavailable = false
    // One project's facts, by project id — the pane's data, fetched per open.
    // Nil means "not loaded yet"; an empty ProjectDetail.facts means the
    // project genuinely has none.
    @Published private(set) var projectDetails: [String: ProjectDetail] = [:]
    // Automation names with a manual run this client started still settling —
    // what disables the Run now button between the POST and the poll's end.
    @Published private(set) var manualRunsInFlight: Set<String> = []
    @Published var pendingBotIDs: Set<String> = []
    @Published var compactingBotIDs: Set<String> = []
    // Unsent composer text, per bot (botId → draft), in-memory only. Not
    // @Published on purpose: ChatView reads and writes it imperatively on bot
    // switches, and it must never drive a view update.
    var composerDrafts: [String: String] = [:]
    @Published var lastError: String?
    @Published var serverReachable = true

    private let api = APIClient()

    func refresh() async {
        do {
            bots = try await api.listBots()
            models = try await api.listModels()
            serverReachable = true
        } catch {
            serverReachable = false
            lastError = "Can't reach botnetd — is the server running? (\(error.localizedDescription))"
            return
        }
        await prefetchConversations()
        await refreshEvents()
        await refreshAutomations()
        await refreshProjects()
    }

    // A server that denormalizes the preview onto the bot has already told us
    // everything the sidebar needs. Only fall back to fetching every
    // conversation for one that hasn't, which costs a request per bot.
    private func prefetchConversations() async {
        let stale = bots.filter { $0.lastMessageText == nil && conversations[$0.id] == nil }
        guard !stale.isEmpty else { return }
        let api = self.api
        let ids = stale.map(\.id)
        let loaded = await withTaskGroup(of: (String, [Message]?).self) { group in
            for id in ids {
                group.addTask { (id, try? await api.messages(id)) }
            }
            var out: [String: [Message]] = [:]
            for await (id, messages) in group {
                if let messages { out[id] = messages }
            }
            return out
        }
        for (id, messages) in loaded { conversations[id] = messages }
    }

    /// The sidebar's one-line preview, preferring the server's denormalized copy
    /// and falling back to a loaded conversation.
    func preview(for bot: Bot) -> String? {
        if let text = bot.lastMessageText, !text.isEmpty { return text }
        return conversations[bot.id]?.last?.content
    }

    func lastActivity(for bot: Bot) -> Date? {
        bot.lastActivity ?? conversations[bot.id]?.last?.sentAt
    }

    func createBot(displayName: String, systemPrompt: String, model: String) async {
        do {
            _ = try await api.createBot(displayName: displayName, systemPrompt: systemPrompt, model: model)
            bots = try await api.listBots()
        } catch {
            lastError = error.localizedDescription
        }
    }

    func deleteBot(_ bot: Bot) async {
        do {
            try await api.deleteBot(bot.id)
            conversations[bot.id] = nil
            bots = try await api.listBots()
        } catch {
            lastError = error.localizedDescription
        }
    }

    func loadConversation(_ botID: String) async {
        do {
            conversations[botID] = try await api.messages(botID)
        } catch {
            lastError = error.localizedDescription
        }
    }

    func messages(for botID: String) -> [Message] { conversations[botID] ?? [] }

    func lastMessage(for botID: String) -> Message? { conversations[botID]?.last }

    func segmentChain(for botID: String) -> [Segment] { segments[botID] ?? [] }

    // Segments, compaction, read state and bot edits all landed after the first
    // release of the server. On a botnetd that predates them the route is a 404,
    // which means "this server can't do that" and must stay silent rather than
    // raising an error the user can do nothing about.
    func loadSegments(_ botID: String) async {
        do {
            segments[botID] = try await api.segments(botID)
        } catch {
            guard !APIClient.isUnimplemented(error) else { return }
            lastError = error.localizedDescription
        }
    }

    // The tool list is deploy-static — it changes only with the server binary —
    // so one fetch per run is enough. A 404 pins tools nil for the run; a
    // transient failure leaves it retryable on the next inspector appearance.
    func loadTools() async {
        guard tools == nil, !toolsUnavailable else { return }
        do {
            tools = try await api.listTools()
        } catch {
            guard !APIClient.isUnimplemented(error) else {
                toolsUnavailable = true
                return
            }
            lastError = error.localizedDescription
        }
    }
    private var toolsUnavailable = false

    // MARK: calendar
    //
    // The server owns the events exactly as it owns the bots; these calls cache
    // its answers and nothing else. A mutation splices the returned Event into
    // the cache rather than refetching the collection: the response IS the
    // server's new row, so a refetch would only cost a round trip.

    func refreshEvents() async {
        do {
            events = try await api.listEvents()
        } catch {
            guard !APIClient.isUnimplemented(error) else { return }
            lastError = error.localizedDescription
        }
        // Events and calendars go stale together — a bot's tool call can make a
        // calendar in the same turn it books into it — so every event refresh
        // re-reads both. Instances derive from both, so they refresh last.
        await refreshCalendars()
        await refreshInstances()
    }

    // The instance fetch window. One window feeds both the list and the month
    // grid — they render the same filtered array, and two windows would let
    // the two readings disagree. A month of "Earlier", six months of future:
    // enough for the grid to show a recurring series repeating, and 210 days
    // stays well under the contract's 400-day cap.
    private static let instanceWindowPast: TimeInterval = 30 * 86_400
    private static let instanceWindowFuture: TimeInterval = 180 * 86_400

    /// Re-derives the pane's instances. Runs after every event fetch and every
    /// event mutation: instances are expanded server-side, so a saved edit to a
    /// recurring master moves occurrences this cache can't compute locally.
    func refreshInstances() async {
        let today = Calendar.current.startOfDay(for: Date())
        do {
            instances = try await api.listInstances(
                from: today.addingTimeInterval(-Self.instanceWindowPast),
                to: today.addingTimeInterval(Self.instanceWindowFuture))
        } catch {
            guard !APIClient.isUnimplemented(error) else {
                // Old server: nothing expands, so the wholesale events ARE the
                // instances — including ones outside the window, exactly the
                // pane's pre-instances behavior.
                instances = events.map(EventInstance.single)
                return
            }
            lastError = error.localizedDescription
        }
    }

    /// The master event an instance points at, for opening the editor. Nil only
    /// if the master vanished between the two fetches; the caller then does
    /// nothing rather than editing a ghost.
    func event(id: String) -> Event? {
        events.first { $0.id == id }
    }

    func refreshCalendars() async {
        do {
            calendars = try await api.listCalendars()
            calendarsUnavailable = false
        } catch {
            guard !APIClient.isUnimplemented(error) else {
                // Not sticky by design: a restart onto a current botnetd mid-run
                // brings the chrome back on the next refresh instead of
                // pinning the old-server look for the whole session.
                calendars = []
                calendarsUnavailable = true
                return
            }
            lastError = error.localizedDescription
        }
    }

    /// The calendar an event files under, resolved against the live list at
    /// render time — a recolor propagates to every row without a refetch. Nil
    /// for an old server's events and for a calendar deleted since the fetch.
    func calendar(id: String?) -> EventCalendar? {
        guard let id else { return nil }
        return calendars.first { $0.id == id }
    }

    /// How many cached events file under a calendar; what the delete
    /// confirmation counts.
    func eventCount(in calendar: EventCalendar) -> Int {
        events.filter { $0.calendarId == calendar.id }.count
    }

    @discardableResult
    func createCalendar(name: String, color: String?) async -> Bool {
        do {
            mergeCalendar(try await api.createCalendar(name: name, color: color))
            return true
        } catch {
            lastError = calendarError(error, verb: "add a calendar")
            return false
        }
    }

    @discardableResult
    func updateCalendar(_ calendar: EventCalendar, fields: [String: String]) async -> Bool {
        guard !fields.isEmpty else { return true }
        do {
            mergeCalendar(try await api.updateCalendar(calendar.id, fields: fields))
            return true
        } catch {
            lastError = calendarError(error, verb: "edit a calendar")
            return false
        }
    }

    /// The server cascades: the calendar's events go with it, so the cache
    /// drops them too rather than stranding rows that point at a dead id.
    func deleteCalendar(_ calendar: EventCalendar) async {
        do {
            try await api.deleteCalendar(calendar.id)
            calendars.removeAll { $0.id == calendar.id }
            events.removeAll { $0.calendarId == calendar.id }
            instances.removeAll { $0.calendarId == calendar.id }
        } catch {
            lastError = calendarError(error, verb: "delete a calendar")
        }
    }

    // Keeps the cache in the server's own order (createdAt ascending, id as the
    // tiebreak), same splice as merge(_:) below and for the same reason.
    private func mergeCalendar(_ calendar: EventCalendar) {
        calendars.removeAll { $0.id == calendar.id }
        let at = calendars.firstIndex {
            ($0.createdAt, $0.id) > (calendar.createdAt, calendar.id)
        } ?? calendars.endIndex
        calendars.insert(calendar, at: at)
    }

    /// Returns whether the create landed, so the sheet can stay open with the
    /// draft intact when it didn't.
    @discardableResult
    func createEvent(title: String, startsAt: Date, endsAt: Date,
                     location: String, notes: String, calendarId: String? = nil) async -> Bool {
        do {
            let created = try await api.createEvent(
                title: title, startsAt: startsAt, endsAt: endsAt,
                location: location, notes: notes, calendarId: calendarId)
            merge(created)
            await refreshInstances()
            return true
        } catch {
            lastError = calendarError(error, verb: "add an event")
            return false
        }
    }

    /// `fields` carries only what the editor changed, so a bot's concurrent
    /// write to another field survives the save.
    @discardableResult
    func updateEvent(_ event: Event, fields: [String: String]) async -> Bool {
        guard !fields.isEmpty else { return true }
        do {
            merge(try await api.updateEvent(event.id, fields: fields))
            await refreshInstances()
            return true
        } catch {
            lastError = calendarError(error, verb: "edit an event")
            return false
        }
    }

    func deleteEvent(_ event: Event) async {
        do {
            try await api.deleteEvent(event.id)
            events.removeAll { $0.id == event.id }
            // The server deletes the whole series (a recurring event is one
            // row), so every instance pointing at it goes too.
            instances.removeAll { $0.eventId == event.id }
        } catch {
            lastError = calendarError(error, verb: "delete an event")
        }
    }

    // Keeps the cache in the server's own order (startsAt ascending, id as the
    // tiebreak) so an edited event moves to its new day without a refetch.
    private func merge(_ event: Event) {
        events.removeAll { $0.id == event.id }
        let at = events.firstIndex {
            ($0.startsAt, $0.id) > (event.startsAt, event.id)
        } ?? events.endIndex
        events.insert(event, at: at)
    }

    private func calendarError(_ error: Error, verb: String) -> String {
        APIClient.isUnimplemented(error)
            ? "This botnetd is too old to \(verb) — restart it from the current build."
            : error.localizedDescription
    }

    // MARK: automations
    //
    // The stdd automations service owns the registry and the runs; botnet
    // only bridges its read/run routes through. Same caching stance as the
    // calendar: these calls hold the server's answers for the views and
    // nothing else.

    func refreshAutomations() async {
        do {
            automations = try await api.listAutomations()
            automationsUnavailable = false
        } catch {
            guard !APIClient.isUnimplemented(error) else {
                automations = []
                automationsUnavailable = true
                return
            }
            lastError = error.localizedDescription
        }
    }

    /// Re-reads one automation with its runs (the detail row) and splices it
    /// over the list row, so the pane and the sidebar dot refresh together.
    func loadAutomationDetail(_ name: String) async {
        do {
            var detail = try await api.automationDetail(name)
            // The wire omits `runs` entirely for an automation with zero runs
            // (Go's omitempty), and the decoder honestly leaves that nil. THIS
            // is the one place absence provably means "no runs yet" — the
            // detail endpoint always reports runs — so normalize here, and the
            // pane's nil keeps meaning "detail not loaded yet".
            detail.runs = detail.runs ?? []
            if let i = automations.firstIndex(where: { $0.name == detail.name }) {
                automations[i] = detail
            } else {
                automations.append(detail)
            }
        } catch {
            guard !APIClient.isUnimplemented(error) else { return }
            lastError = error.localizedDescription
        }
    }

    /// Fetches one run's full row for inline disclosure. Finished runs are
    /// immutable server-side, so a cached one is returned without a refetch.
    func loadRunDetail(_ id: String) async {
        if let cached = runDetails[id], cached.summary.isFinished { return }
        do {
            runDetails[id] = try await api.runDetail(id)
        } catch {
            guard !APIClient.isUnimplemented(error) else { return }
            lastError = error.localizedDescription
        }
    }

    /// Starts a manual run and waits for it to settle, then re-reads the
    /// detail so the runs list and freshness reflect the outcome. Returns a
    /// short transient notice for the pane to flash (a 409 is "already
    /// running", not an error state), or nil when nothing needs saying.
    func runNow(_ name: String) async -> String? {
        guard !manualRunsInFlight.contains(name) else { return nil }
        manualRunsInFlight.insert(name)
        defer { manualRunsInFlight.remove(name) }
        do {
            let id = try await api.runAutomation(name)
            await pollRun(id)
            await loadAutomationDetail(name)
            return nil
        } catch {
            // Either way the server knows more than the cache does now.
            await loadAutomationDetail(name)
            guard !APIClient.isBusy(error) else {
                return "A run is already in flight — showing it below."
            }
            lastError = error.localizedDescription
            return nil
        }
    }

    /// Polls a started run until `finished` is non-empty, per the contract.
    /// Giving up on the deadline leaves the run visibly unfinished — the
    /// service owns that state and the next detail load settles it — same
    /// stance as awaitReply below.
    private func pollRun(_ id: String) async {
        let deadline = Date().addingTimeInterval(Self.runTimeout)
        while Date() < deadline {
            try? await Task.sleep(nanoseconds: 1_000_000_000)
            guard let run = try? await api.runDetail(id) else { continue }
            runDetails[id] = run
            if run.summary.isFinished { return }
        }
    }

    /// Comfortably past the runner's own subprocess timeout, so the service
    /// decides how a stuck run ends, not this poll.
    private static let runTimeout: TimeInterval = 900

    // MARK: projects
    //
    // The server owns the projects and their facts, and DERIVES health, nextDue
    // and factCount from the facts on every read. So every fact write is
    // followed by a re-read rather than a local splice: the write's own response
    // is one fact, and the numbers that moved because of it live on the project
    // and on the list's ordering, which only the server can restate.

    func refreshProjects() async {
        do {
            projects = try await api.listProjects()
            projectsUnavailable = false
        } catch {
            guard !APIClient.isUnimplemented(error) else {
                projects = []
                projectsUnavailable = true
                return
            }
            lastError = error.localizedDescription
        }
    }

    /// Re-reads one project with its facts (the pane's data) and splices the
    /// returned row over the list entry, so the header badge and the sidebar
    /// dot move together.
    func loadProject(_ id: String) async {
        do {
            let detail = try await api.project(id)
            projectDetails[id] = detail
            if let i = projects.firstIndex(where: { $0.id == id }) {
                projects[i] = detail.project
            }
        } catch {
            guard !APIClient.isUnimplemented(error) else { return }
            lastError = error.localizedDescription
        }
    }

    /// The pane's facts. Nil until the first load — which is what tells
    /// "still fetching" apart from "this project has no facts".
    func facts(for projectID: String) -> [ProjectFact]? {
        projectDetails[projectID]?.facts
    }

    /// Returns the created project so the caller can select it; nil when the
    /// create failed, which keeps the sheet open with its draft.
    func createProject(name: String, goal: String, parentID: String = "") async -> Project? {
        do {
            let created = try await api.createProject(name: name, goal: goal, parentID: parentID)
            await refreshProjects()
            // The parent's own detail now has one more child and possibly a
            // worse rolled-up severity; its cached copy has neither.
            if !parentID.isEmpty { await loadProject(parentID) }
            return created
        } catch {
            lastError = projectError(error, verb: "add a project")
            return nil
        }
    }

    /// `fields` is only what the editor changed — projects are last-write-wins
    /// like events, so resending an untouched field would clobber whatever a
    /// bot's tool wrote to it while the sheet was open.
    @discardableResult
    func updateProject(_ project: Project, fields: [String: String]) async -> Bool {
        guard !fields.isEmpty else { return true }
        do {
            let updated = try await api.updateProject(project.id, fields: fields)
            if let i = projects.firstIndex(where: { $0.id == updated.id }) {
                projects[i] = updated
            }
            if var detail = projectDetails[updated.id] {
                detail.project = updated
                projectDetails[updated.id] = detail
            }
            // A rename re-sorts the list at equal health, and the server also
            // rewrites the projected events' titles — re-read rather than guess.
            await refreshProjects()
            // A MOVE changes two other projects: the parent it left and the one
            // it joined each gain or lose a child and re-roll their severity.
            if fields["parentId"] != nil {
                for parent in Set([project.parentId, updated.parentId].compactMap { $0 }) {
                    await loadProject(parent)
                }
            }
            return true
        } catch {
            lastError = projectError(error, verb: "edit a project")
            return false
        }
    }

    /// The server cascades the WHOLE SUBTREE: descendant projects, every fact
    /// under them and their projected calendar events all go. Which rows that
    /// removed is the server's answer, not a local guess, so the list is
    /// re-read rather than spliced — and the parent it hung under re-rolls its
    /// severity from what is left.
    func deleteProject(_ project: Project) async {
        do {
            let subtree = ProjectTree(projects).subtree(of: project.id).map(\.id)
            try await api.deleteProject(project.id)
            for id in subtree.isEmpty ? [project.id] : subtree { projectDetails[id] = nil }
            await refreshProjects()
            if let parent = project.parentId { await loadProject(parent) }
            await refreshEvents()
        } catch {
            lastError = projectError(error, verb: "delete a project")
        }
    }

    /// Returns whether the fact landed, so the sheet can stay open with the
    /// draft intact when it didn't.
    @discardableResult
    func addFact(to projectID: String, fields: [String: Any]) async -> Bool {
        do {
            _ = try await api.createFact(projectID, fields: fields)
            await reload(projectID)
            return true
        } catch {
            lastError = projectError(error, verb: "add a fact")
            return false
        }
    }

    @discardableResult
    func updateFact(_ fact: ProjectFact, fields: [String: Any]) async -> Bool {
        guard !fields.isEmpty else { return true }
        do {
            _ = try await api.updateFact(fact.projectId, factID: fact.id, fields: fields)
            await reload(fact.projectId)
            return true
        } catch {
            lastError = projectError(error, verb: "edit a fact")
            return false
        }
    }

    func deleteFact(_ fact: ProjectFact) async {
        do {
            try await api.deleteFact(fact.projectId, factID: fact.id)
            await reload(fact.projectId)
        } catch {
            lastError = projectError(error, verb: "delete a fact")
        }
    }

    /// After a fact write: the pane's facts, the project's derived health, and
    /// the list's health-precedence ordering are all stale at once, and the
    /// projected calendar event may have been created, moved or deleted.
    private func reload(_ projectID: String) async {
        await loadProject(projectID)
        await refreshProjects()
        await refreshEvents()
    }

    private func projectError(_ error: Error, verb: String) -> String {
        APIClient.isUnimplemented(error)
            ? "This botnetd is too old to \(verb) — restart it from the current build."
            : error.localizedDescription
    }

    func compact(_ bot: Bot) async {
        guard !compactingBotIDs.contains(bot.id) else { return }
        compactingBotIDs.insert(bot.id)
        defer { compactingBotIDs.remove(bot.id) }
        do {
            segments[bot.id] = try await api.compact(bot.id)
            conversations[bot.id] = try await api.messages(bot.id)
        } catch {
            lastError = APIClient.isUnimplemented(error)
                ? "This botnetd is too old to compact — restart it from the current build."
                : error.localizedDescription
        }
    }

    func markRead(_ bot: Bot) async {
        guard bot.hasUnread else { return }
        do {
            let updated = try await api.markRead(bot.id)
            if let i = bots.firstIndex(where: { $0.id == updated.id }) { bots[i] = updated }
        } catch {
            guard !APIClient.isUnimplemented(error) else { return }
            lastError = error.localizedDescription
        }
    }

    // Returns whether the patch landed, so an editor can hold onto unsaved text
    // when it didn't.
    @discardableResult
    func updateBot(_ bot: Bot, fields: [String: String]) async -> Bool {
        do {
            _ = try await api.patchBot(bot.id, fields: fields)
            bots = try await api.listBots()
            return true
        } catch {
            lastError = APIClient.isUnimplemented(error)
                ? "This botnetd is too old to edit a bot — restart it from the current build."
                : error.localizedDescription
            return false
        }
    }

    func hasServerKey() async throws -> Bool { try await api.hasKey() }

    func setServerKey(_ key: String) async {
        do { try await api.setKey(key) } catch { lastError = error.localizedDescription }
    }

    // The chat lifecycle lives server-side; the client just posts and renders
    // whatever conversation comes back. A failed send is not an exception here:
    // the server persists the user's turn either way and hands back the
    // transcript with it, so the stranded message renders in place and the
    // failure shows on the turn rather than in a modal.
    func send(_ text: String, to bot: Bot) async {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }

        // Draw the user's turn before the request goes out. The send blocks for
        // as long as the model takes — measured at 8s — and until it returns the
        // message exists nowhere the transcript can render, so the UI looked
        // like it had swallowed the text. This placeholder is replaced by the
        // server's own copy, id and all, the moment anything comes back.
        let placeholder = Message.placeholder(content: trimmed, botID: bot.id)
        conversations[bot.id, default: []].append(placeholder)

        pendingBotIDs.insert(bot.id)
        defer { pendingBotIDs.remove(bot.id) }
        do {
            let persisted = try await api.send(trimmed, to: bot.id)
            replace(placeholder.id, with: persisted, in: bot.id)
            await awaitReply(to: persisted, on: bot.id)
        } catch {
            lastError = error.localizedDescription
            // Replace wholesale rather than dropping the placeholder, so the
            // transcript matches the server even if it did persist the turn.
            conversations[bot.id] = try? await api.messages(bot.id)
        }
    }

    func retry(_ messageID: String, on botID: String) async {
        pendingBotIDs.insert(botID)
        defer { pendingBotIDs.remove(botID) }
        do {
            let reopened = try await api.retry(messageID, on: botID)
            replace(messageID, with: reopened, in: botID)
            await awaitReply(to: reopened, on: botID)
        } catch {
            lastError = error.localizedDescription
            conversations[botID] = try? await api.messages(botID)
        }
    }

    /// Polls until the turn settles. The send returns in milliseconds now, so
    /// this is where the model's actual latency is spent; the turn stays
    /// `awaiting` in the transcript throughout, which is what the user sees.
    ///
    /// Giving up on the deadline deliberately leaves the message awaiting rather
    /// than faking a failure: the server owns that state, and its startup sweep
    /// or an explicit retry is what settles it.
    private func awaitReply(to turn: Message, on botID: String) async {
        let deadline = Date().addingTimeInterval(Self.replyTimeout)
        while Date() < deadline {
            try? await Task.sleep(nanoseconds: 600_000_000)
            guard let settled = try? await api.message(turn.id) else { continue }
            guard settled.status != .awaiting else { continue }

            replace(turn.id, with: settled, in: botID)
            if let reply = try? await api.messages(botID, after: turn.id) {
                appendMissing(reply, in: botID)
            }
            await refreshBotList()
            // A reply can have run the calendar tool mid-turn, which leaves the
            // cached events as stale as the sidebar preview and for the same
            // reason: the server wrote and nobody told the client.
            await refreshEvents()
            // Freshness dots age the same way: a scheduled fire can land while
            // the user chats, and this settle is the one refresh moment.
            await refreshAutomations()
            // And the reply may have run the project tool, which moves health
            // and nextDue on rows the sidebar is already drawing.
            await refreshProjects()
            return
        }
    }

    private static let replyTimeout: TimeInterval = 180

    private func replace(_ messageID: String, with message: Message, in botID: String) {
        guard var thread = conversations[botID] else { return }
        if let i = thread.firstIndex(where: { $0.id == messageID }) {
            thread[i] = message
        } else {
            thread.append(message)
        }
        conversations[botID] = thread
    }

    private func appendMissing(_ incoming: [Message], in botID: String) {
        var thread = conversations[botID] ?? []
        let known = Set(thread.map(\.id))
        thread.append(contentsOf: incoming.filter { !known.contains($0.id) })
        conversations[botID] = thread
    }

    // The sidebar's preview and ordering are server-derived, so they go stale on
    // every send until the bot list is re-read. The model can also write the
    // bot's memory while composing its reply, so this same re-read is what
    // updates an open memory panel the moment the reply settles — awaitReply
    // must keep calling it.
    private func refreshBotList() async {
        bots = (try? await api.listBots()) ?? bots
    }
}
