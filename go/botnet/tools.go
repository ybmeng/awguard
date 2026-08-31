package botnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The model's tool surface: ONE tool named "memory", Anthropic-memory-tool
// style — a FLAT schema, a strict "command" enum plus an optional "content"
// string. No nested oneOf/discriminated unions: mid-tier models handle a flat
// schema with a prose description far better.
//
// memoryCommands below is THE registry: one entry declares a command's name,
// whether it requires content, the description line advertised to the model,
// and its executor. The enum, the tool description and the dispatch are all
// derived from the table, so a future command (append and list operations are
// planned) is one appended entry — no switch statements to keep in step.
//
// Executions are server-side, mid-turn, and unconditional (no If-Match): each
// is one atomic store write, captured into change_log by the schema's triggers
// like any other write. A malformed call — unknown command, missing content —
// answers the model with an instructive "error: ..." tool result it can
// self-correct from; that consumes a loop iteration but does not fail the
// turn. Only a real store failure fails the turn.

// maxToolIterations caps one turn's model↔tool loop. A model stuck calling
// tools forever would otherwise hold the bot's single in-flight slot until the
// turn timeout; at the cap the turn settles as failed, naming the cap.
const maxToolIterations = 8

// memoryToolName is the one tool the request advertises.
const memoryToolName = "memory"

// memoryCommand declares one command of the memory tool.
type memoryCommand struct {
	name         string
	needsContent bool   // "content" is required; its absence is an instructive error
	doc          string // the description line advertised to the model
	run          func(s *Store, botID BotID, content string) (string, error)
}

// memoryCommands is the registry — the single place a command is declared.
var memoryCommands = []memoryCommand{
	{
		name: "read",
		doc: `"read": returns your memory verbatim. Your memory is already shown in your ` +
			`context, so read is only needed to re-check it after an edit this turn. Takes no other fields.`,
		run: func(s *Store, botID BotID, _ string) (string, error) {
			bot, err := s.GetBot(botID)
			if err != nil {
				return "", err
			}
			if bot.Memory == "" {
				return "(your memory is empty)", nil
			}
			return bot.Memory, nil
		},
	},
	{
		name:         "replace",
		needsContent: true,
		doc: `"replace": requires "content" and overwrites your ENTIRE memory with it, ` +
			`so include everything worth keeping.`,
		run: func(s *Store, botID BotID, content string) (string, error) {
			if _, err := s.SetMemory(botID, content); err != nil {
				return "", err
			}
			return "memory saved", nil
		},
	},
	{
		name: "clear",
		doc:  `"clear": erases your memory entirely. Takes no other fields.`,
		run: func(s *Store, botID BotID, _ string) (string, error) {
			if _, err := s.SetMemory(botID, ""); err != nil {
				return "", err
			}
			return "memory cleared", nil
		},
	},
}

// commandNames lists the enum, in registry order.
func commandNames() []string {
	names := make([]string, len(memoryCommands))
	for i, c := range memoryCommands {
		names[i] = c.name
	}
	return names
}

// memoryToolDef renders the registry as the one wire tool definition: the
// description spells out each command's requirements in prose, and the enum
// is strict.
func memoryToolDef() wireTool {
	lines := []string{"Manage your persistent memory. Commands:"}
	for _, c := range memoryCommands {
		lines = append(lines, "- "+c.doc)
	}
	return wireTool{Type: "function", Function: wireToolFunction{
		Name:        memoryToolName,
		Description: strings.Join(lines, "\n"),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"enum":        commandNames(),
					"description": "The operation to perform.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": `The full new memory, for "replace" only. Omit for other commands.`,
				},
			},
			"required": []string{"command"},
		},
	}}
}

// ── calendar ──────────────────────────────────────────────────────────────────
// The bot's half of the calendar service: the same command-registry shape the
// memory tool uses, over the events table. calendarCommands is THE registry —
// the enum, the advertised description and the dispatch all derive from it, so
// a fifth command is one appended entry.
//
// The arguments are FLAT strings for the same reason memory's are: a nested
// union of per-command shapes is exactly what mid-tier models get wrong. Which
// fields a command needs is prose in its description plus the `requires` list
// here, and a call that misses one gets an instructive "error: ..." RESULT
// naming the field rather than failing the turn.

// calendarToolName is the tool the model calls to read and write the calendar.
const calendarToolName = "calendar"

// calendarDefaultDuration is what an event gets when the model gives a start
// but no end. A model asked to "book lunch at noon" reliably omits the end, and
// refusing that call would cost a whole loop iteration to learn nothing.
const calendarDefaultDuration = time.Hour

// calendarDefaultWindow is how far ahead a bare "list" looks. Unbounded would
// dump a year of calendar into the context to answer "what's coming up".
const calendarDefaultWindow = 14 * 24 * time.Hour

// calendarArgs is the flat argument object every command shares. The optional
// fields are POINTERS so an absent field and an explicitly empty one are
// different answers — clearing a location on update is "location": "", which
// must not read the same as leaving it alone.
type calendarArgs struct {
	Command  string  `json:"command"`
	EventID  string  `json:"event_id"`
	Title    *string `json:"title"`
	Start    *string `json:"start"`
	End      *string `json:"end"`
	Location *string `json:"location"`
	Notes    *string `json:"notes"`
	From     *string `json:"from"`
	To       *string `json:"to"`
	Calendar *string `json:"calendar"` // an existing calendar, by name
	Name     *string `json:"name"`     // a calendar's (new) name, for the *_calendar commands
	Color    *string `json:"color"`    // a calendar color, for the *_calendar commands
}

// field returns one flat field by its wire name, and whether it was supplied
// with a non-empty value. It is what lets `requires` be a list of names rather
// than a per-command branch.
func (a calendarArgs) field(name string) (string, bool) {
	var p *string
	switch name {
	case "event_id":
		if a.EventID == "" {
			return "", false
		}
		return a.EventID, true
	case "title":
		p = a.Title
	case "start":
		p = a.Start
	case "end":
		p = a.End
	case "location":
		p = a.Location
	case "notes":
		p = a.Notes
	case "from":
		p = a.From
	case "to":
		p = a.To
	case "calendar":
		p = a.Calendar
	case "name":
		p = a.Name
	case "color":
		p = a.Color
	}
	if p == nil || *p == "" {
		return "", false
	}
	return *p, true
}

// calendarCommand declares one command of the calendar tool.
type calendarCommand struct {
	name     string
	requires []string // flat fields that must be present and non-empty
	doc      string   // the description line advertised to the model
	run      func(s *Store, botID BotID, a calendarArgs) (string, error)
}

// calendarCommands is the registry — the single place a command is declared.
var calendarCommands = []calendarCommand{
	{
		name:     "create",
		requires: []string{"title", "start"},
		doc: `"create": books an event. Requires "title" and "start"; optional "end" ` +
			`(defaults to one hour after the start), "location", "notes" and "calendar" ` +
			`(a calendar name; defaults to "Personal").`,
		run: runCalendarCreate,
	},
	{
		name: "list",
		doc: `"list": shows the calendar. Optional "from" and "to" bound the window; ` +
			`it defaults to the next 14 days. Optional "calendar" (a calendar name) shows ` +
			`only that calendar. The first line is always the current time, ` +
			`so use it to resolve "today", "tomorrow" and "next week".`,
		run: runCalendarList,
	},
	{
		name:     "update",
		requires: []string{"event_id"},
		doc: `"update": changes an existing event. Requires "event_id" (from "list") plus ` +
			`any of "title", "start", "end", "location", "notes", "calendar" (moves it to ` +
			`that calendar) — omitted fields are left alone.`,
		run: runCalendarUpdate,
	},
	{
		name:     "delete",
		requires: []string{"event_id"},
		doc:      `"delete": cancels an event. Requires "event_id" (from "list").`,
		run:      runCalendarDelete,
	},
	{
		name:     "create_calendar",
		requires: []string{"name"},
		doc: `"create_calendar": adds a named calendar. Requires "name"; optional "color" ` +
			`(one of ` + strings.Join(calendarColors, ", ") + ` — assigned automatically when omitted).`,
		run: runCalendarCreateCalendar,
	},
	{
		name: "list_calendars",
		doc:  `"list_calendars": shows every calendar with its color and event count.`,
		run:  runCalendarListCalendars,
	},
	{
		name:     "rename_calendar",
		requires: []string{"calendar"},
		doc: `"rename_calendar": renames or recolors a calendar. Requires "calendar" (its ` +
			`current name) plus "name" (the new name) and/or "color".`,
		run: runCalendarRenameCalendar,
	},
	{
		name:     "delete_calendar",
		requires: []string{"calendar"},
		doc: `"delete_calendar": removes an EMPTY calendar. Requires "calendar" (its name). ` +
			`A calendar that still has events is refused — delete or move them first.`,
		run: runCalendarDeleteCalendar,
	},
}

// calendarCommandNames lists the enum, in registry order.
func calendarCommandNames() []string {
	names := make([]string, len(calendarCommands))
	for i, c := range calendarCommands {
		names[i] = c.name
	}
	return names
}

// calendarToolDef renders the registry as the wire tool definition, the same
// way memoryToolDef does: prose per command, one strict enum, flat strings.
func calendarToolDef() wireTool {
	lines := []string{
		"Read and write the shared calendar. You, the user and the other bots all see the same events, " +
			"organized into named calendars (e.g. \"Personal\", \"Company Earnings\"). " +
			"All times are RFC3339 (e.g. 2026-08-31T15:00:00Z or 2026-08-31T15:00:00-07:00). Commands:",
	}
	for _, c := range calendarCommands {
		lines = append(lines, "- "+c.doc)
	}
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	return wireTool{Type: "function", Function: wireToolFunction{
		Name:        calendarToolName,
		Description: strings.Join(lines, "\n"),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"enum":        calendarCommandNames(),
					"description": "The operation to perform.",
				},
				"event_id": str(`The event to change, for "update" and "delete". Take it from "list".`),
				"title":    str(`The event's title, for "create" and "update".`),
				"start":    str(`When the event starts, RFC3339, for "create" and "update".`),
				"end":      str(`When the event ends, RFC3339. Optional; "create" defaults to one hour after the start.`),
				"location": str(`Where it happens. Optional.`),
				"notes":    str(`Anything else worth keeping on the event. Optional.`),
				"from":     str(`Start of the window to list, RFC3339. Optional; defaults to now.`),
				"to":       str(`End of the window to list, RFC3339. Optional; defaults to 14 days out.`),
				"calendar": str(`A calendar, by name (case-insensitive). Optional filter/target for "create", "update" and "list"; the current name for "rename_calendar" and "delete_calendar".`),
				"name":     str(`A calendar's name: the new calendar for "create_calendar", the new name for "rename_calendar".`),
				"color":    str(`A calendar color, one of ` + strings.Join(calendarColors, ", ") + `, for "create_calendar" and "rename_calendar". Optional.`),
			},
			"required": []string{"command"},
		},
	}}
}

// calendarTime parses one RFC3339 field, naming the field in the instructive
// error the model self-corrects from.
func calendarTime(field, raw string) (time.Time, error) {
	t, err := parseEventTime(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("error: '%s' value %q is not an RFC3339 time like 2026-08-31T15:00:00Z", field, raw)
	}
	return t, nil
}

// calendarNamed resolves the "calendar" name arg. An unknown name answers with
// the instructive error listing the calendars that DO exist — deliberately not
// auto-creating, because a typo must not spawn a calendar. The three returns
// are the tool-handler split: a calendar, an instructive error text for the
// model, or a real store failure.
func calendarNamed(s *Store, name string) (Calendar, string, error) {
	cal, err := s.CalendarByName(name)
	if err == nil {
		return cal, "", nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Calendar{}, "", err
	}
	cals, err := s.ListCalendars()
	if err != nil {
		return Calendar{}, "", err
	}
	if len(cals) == 0 {
		return Calendar{}, fmt.Sprintf("error: no calendar named %q — none exist yet; create_calendar makes one", name), nil
	}
	names := make([]string, len(cals))
	for i, c := range cals {
		names[i] = c.Name
	}
	return Calendar{}, fmt.Sprintf("error: no calendar named %q — existing calendars: %s",
		name, strings.Join(names, ", ")), nil
}

func runCalendarCreate(s *Store, botID BotID, a calendarArgs) (string, error) {
	title, _ := a.field("title")
	rawStart, _ := a.field("start")
	start, err := calendarTime("start", rawStart)
	if err != nil {
		return err.Error(), nil
	}
	end := start.Add(calendarDefaultDuration)
	if raw, ok := a.field("end"); ok {
		if end, err = calendarTime("end", raw); err != nil {
			return err.Error(), nil
		}
	}
	location, _ := a.field("location")
	notes, _ := a.field("notes")
	// A zero CalendarID gets the store's Personal ensure, so an unqualified
	// booking lands exactly where an unqualified REST create does.
	var calID CalendarID
	if name, ok := a.field("calendar"); ok {
		cal, errText, err := calendarNamed(s, name)
		if err != nil {
			return "", err
		}
		if errText != "" {
			return errText, nil
		}
		calID = cal.ID
	}
	// createdBy is the CALLING bot, stamped by the store — the model cannot
	// name an author, so an event always says who really booked it.
	ev, err := s.CreateEvent(Event{
		Title: title, StartsAt: start, EndsAt: end, Location: location, Notes: notes,
		CalendarID: calID,
	}, string(botID))
	if err != nil {
		return calendarStoreError(err)
	}
	return fmt.Sprintf("created %s: %s %s", ev.ID, ev.Title, localRFC3339(ev.StartsAt)), nil
}

func runCalendarList(s *Store, _ BotID, a calendarArgs) (string, error) {
	now := time.Now()
	from, to := now, now.Add(calendarDefaultWindow)
	if raw, ok := a.field("from"); ok {
		t, err := calendarTime("from", raw)
		if err != nil {
			return err.Error(), nil
		}
		from = t
	}
	if raw, ok := a.field("to"); ok {
		t, err := calendarTime("to", raw)
		if err != nil {
			return err.Error(), nil
		}
		to = t
	}
	var only *Calendar
	if name, ok := a.field("calendar"); ok {
		cal, errText, err := calendarNamed(s, name)
		if err != nil {
			return "", err
		}
		if errText != "" {
			return errText, nil
		}
		only = &cal
	}
	events, err := s.ListEvents(from, to)
	if err != nil {
		return calendarStoreError(err)
	}
	if only != nil {
		kept := events[:0]
		for _, e := range events {
			if e.CalendarID == only.ID {
				kept = append(kept, e)
			}
		}
		events = kept
	}
	// Annotate each event with its calendar's name whenever there is more than
	// one calendar to tell apart — a single-calendar net reads as before.
	cals, err := s.ListCalendars()
	if err != nil {
		return "", err
	}
	var names map[CalendarID]string
	if len(cals) > 1 {
		names = make(map[CalendarID]string, len(cals))
		for _, c := range cals {
			names[c.ID] = c.Name
		}
	}
	return renderCalendar(now, from, to, events, names), nil
}

func runCalendarCreateCalendar(s *Store, botID BotID, a calendarArgs) (string, error) {
	name, _ := a.field("name")
	color, _ := a.field("color")
	// createdBy is the calling bot, exactly as event creates stamp it.
	cal, err := s.CreateCalendar(name, color, string(botID))
	if err != nil {
		return calendarStoreError(err)
	}
	return fmt.Sprintf("created calendar %q (%s)", cal.Name, cal.Color), nil
}

func runCalendarListCalendars(s *Store, _ BotID, _ calendarArgs) (string, error) {
	cals, err := s.ListCalendars()
	if err != nil {
		return "", err
	}
	if len(cals) == 0 {
		return "(no calendars yet — create_calendar makes one)", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d calendar(s):\n", len(cals))
	for i, c := range cals {
		n, err := s.EventCount(c.ID)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%d. %s (%s) — %d event(s)\n", i+1, c.Name, c.Color, n)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func runCalendarRenameCalendar(s *Store, _ BotID, a calendarArgs) (string, error) {
	current, _ := a.field("calendar")
	cal, errText, err := calendarNamed(s, current)
	if err != nil {
		return "", err
	}
	if errText != "" {
		return errText, nil
	}
	var p CalendarPatch
	if v, ok := a.field("name"); ok {
		p.Name = &v
	}
	if v, ok := a.field("color"); ok {
		p.Color = &v
	}
	if p.Name == nil && p.Color == nil {
		return "error: 'rename_calendar' needs a 'name' (the new name) or a 'color' to change", nil
	}
	updated, err := s.UpdateCalendar(cal.ID, p)
	if err != nil {
		return calendarStoreError(err)
	}
	return fmt.Sprintf("updated calendar %q (%s)", updated.Name, updated.Color), nil
}

// runCalendarDeleteCalendar refuses a non-empty calendar BY DECISION (see
// Calendar in schema.go): the destructive cascade is the UI's, behind a
// confirmation; a model gets it one event at a time or not at all.
func runCalendarDeleteCalendar(s *Store, _ BotID, a calendarArgs) (string, error) {
	name, _ := a.field("calendar")
	cal, errText, err := calendarNamed(s, name)
	if err != nil {
		return "", err
	}
	if errText != "" {
		return errText, nil
	}
	n, err := s.EventCount(cal.ID)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return fmt.Sprintf("error: calendar %q has %d event(s); delete or move them first", cal.Name, n), nil
	}
	if err := s.DeleteCalendar(cal.ID); err != nil {
		return calendarStoreError(err)
	}
	return fmt.Sprintf("deleted calendar %q", cal.Name), nil
}

func runCalendarUpdate(s *Store, _ BotID, a calendarArgs) (string, error) {
	var p EventPatch
	changed := false
	for _, f := range []struct {
		name string
		set  func(string)
	}{
		{"title", func(v string) { p.Title = &v }},
		{"location", func(v string) { p.Location = &v }},
		{"notes", func(v string) { p.Notes = &v }},
	} {
		if v, ok := a.field(f.name); ok {
			f.set(v)
			changed = true
		}
	}
	for _, f := range []struct {
		name string
		set  func(time.Time)
	}{
		{"start", func(v time.Time) { p.StartsAt = &v }},
		{"end", func(v time.Time) { p.EndsAt = &v }},
	} {
		raw, ok := a.field(f.name)
		if !ok {
			continue
		}
		t, err := calendarTime(f.name, raw)
		if err != nil {
			return err.Error(), nil
		}
		f.set(t)
		changed = true
	}
	if name, ok := a.field("calendar"); ok {
		cal, errText, err := calendarNamed(s, name)
		if err != nil {
			return "", err
		}
		if errText != "" {
			return errText, nil
		}
		p.CalendarID = &cal.ID
		changed = true
	}
	if !changed {
		return "error: 'update' needs at least one of title, start, end, location, notes, calendar to change", nil
	}
	ev, err := s.UpdateEvent(EventID(a.EventID), p)
	if err != nil {
		return calendarStoreError(err)
	}
	return fmt.Sprintf("updated %s: %s %s", ev.ID, ev.Title, localRFC3339(ev.StartsAt)), nil
}

func runCalendarDelete(s *Store, _ BotID, a calendarArgs) (string, error) {
	if err := s.DeleteEvent(EventID(a.EventID)); err != nil {
		return calendarStoreError(err)
	}
	return fmt.Sprintf("deleted %s", a.EventID), nil
}

// calendarStoreError splits the store's answer in two. A missing event or a
// rejected value is the MODEL's mistake, so it comes back as an instructive
// result it can fix on the next iteration; anything else is a real store
// failure and fails the turn, exactly as the memory tool does.
func calendarStoreError(err error) (string, error) {
	switch {
	case errors.Is(err, ErrNotFound):
		return "error: no such event — call list to see the current ids", nil
	case errors.Is(err, ErrInvalid):
		return "error: " + strings.TrimPrefix(err.Error(), "botnet: invalid: "), nil
	default:
		return "", err
	}
}

// localRFC3339 renders a time in the SERVER's zone, which is the user's — the
// same zone the "now:" line reports — so the model compares like with like
// rather than doing UTC arithmetic in its head. Storage stays UTC.
func localRFC3339(t time.Time) string {
	return t.Local().Format(time.RFC3339)
}

// renderCalendar formats a listing: the current time FIRST, always, then a
// compact numbered line per event. The now line is what makes the tool usable
// at all — without it "book lunch tomorrow" has no anchor. A non-nil names map
// annotates each event with its calendar — the caller passes one exactly when
// more than one calendar exists, so a single-calendar net stays uncluttered.
func renderCalendar(now, from, to time.Time, events []Event, names map[CalendarID]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "now: %s\n", localRFC3339(now))
	if len(events) == 0 {
		fmt.Fprintf(&b, "(no events between %s and %s)", localRFC3339(from), localRFC3339(to))
		return b.String()
	}
	fmt.Fprintf(&b, "%d event(s) between %s and %s:\n", len(events), localRFC3339(from), localRFC3339(to))
	for i, e := range events {
		fmt.Fprintf(&b, "%d. %s  %s → %s  %s", i+1, e.ID,
			localRFC3339(e.StartsAt), localRFC3339(e.EndsAt), e.Title)
		if name, ok := names[e.CalendarID]; ok {
			fmt.Fprintf(&b, "  [%s]", name)
		}
		if e.Location != "" {
			fmt.Fprintf(&b, "  @ %s", e.Location)
		}
		fmt.Fprintf(&b, "  (by %s)\n", e.CreatedBy)
		if e.Notes != "" {
			fmt.Fprintf(&b, "   %s\n", e.Notes)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// webSearchToolName is OpenRouter's built-in web-search SERVER tool — the
// no-regression FALLBACK offered only when no client search backend is
// configured. It is resolved by OpenRouter server-side (the search never
// round-trips to us as a call to dispatch), so it stays outside the handler
// registry and Run never sees it.
const webSearchToolName = "openrouter:web_search"

// webSearchFuncName is botnet's own web-search FUNCTION tool — offered when the
// router has ≥1 available backend. Unlike the server tool it DOES round-trip:
// the model hands us the query, Run dispatches it to the active backend, and the
// results are recorded in the turn's ToolCall audit trail.
const webSearchFuncName = "web_search"

// webSearchServerToolDef is the fallback server-tool entry. It carries no
// parameters, so the model searches at its own discretion with OpenRouter's
// defaults.
func webSearchServerToolDef() serverTool {
	return serverTool{Type: webSearchToolName}
}

// webSearchFuncDef is the client function tool: {query, num_results?}. The
// model hands us the query; we run it and feed the results back.
func webSearchFuncDef() wireTool {
	return wireTool{Type: "function", Function: wireToolFunction{
		Name: webSearchFuncName,
		Description: "Search the web for current or external information and get back a ranked " +
			"list of results, each with a title, URL and short excerpt. Use it whenever the " +
			"answer depends on recent events or facts outside your knowledge, then cite the sources.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The search query.",
				},
				"num_results": map[string]any{
					"type":        "integer",
					"description": "How many results to return (optional; defaults to a handful).",
				},
			},
			"required": []string{"query"},
		},
	}}
}

// toolWireDefs renders the tool surface as the chat-completions "tools" array,
// gated on the search router. It always offers the memory and calendar FUNCTION
// tools — both are backed by the server's own store, so there is nothing to gate
// them on; for search it offers EITHER botnet's own web_search function tool
// (when the router has a backend) OR OpenRouter's web_search SERVER tool (the
// fallback) — never both, so the model has exactly one way to search. Returning
// []any lets each entry marshal with exactly its own fields, so the server tool
// never leaks a bogus empty "function" object into the request or into
// /v1/tools.
func toolWireDefs(search *Router) []any {
	defs := []any{memoryToolDef(), calendarToolDef()}
	if search.Available() {
		defs = append(defs, webSearchFuncDef())
	} else {
		defs = append(defs, webSearchServerToolDef())
	}
	return defs
}

// toolResult is what a tool handler produces: the text handed back to the model,
// plus — for web_search only — the backend that ran and the structured sources,
// which the loop folds into the turn's ToolCall audit record and the reply's
// aggregate citations. Memory handlers leave backend, requestID and results zero.
type toolResult struct {
	text      string
	backend   string
	requestID string // web_search only: provider request/response id, "" when none
	results   []Citation
}

// BotToolbox binds the tool surface to one bot in one store — what runTurn hands
// the LLM so the model's tool calls execute against the right bot. search is the
// backend router for web_search; a nil router means no client search backend is
// configured, in which case web_search is never offered and never dispatched
// (the OpenRouter server tool is offered instead — see toolWireDefs).
type BotToolbox struct {
	store  *Store
	botID  BotID
	search *Router
}

// NewBotToolbox builds the tool surface for one bot's turn. A nil search router
// disables the client web_search tool; the server falls back to OpenRouter's
// server tool. Tests that exercise only memory pass nil.
func NewBotToolbox(s *Store, botID BotID, search *Router) *BotToolbox {
	return &BotToolbox{store: s, botID: botID, search: search}
}

// wireDefs is the tool surface this toolbox will actually run, gated on its own
// router — so what the request advertises can never drift from what Run can
// dispatch.
func (tb *BotToolbox) wireDefs() []any { return toolWireDefs(tb.search) }

// toolHandlers is THE dispatch registry: one entry per tool the model can call
// and we resolve ourselves (memory, web_search). Replacing the old single-name
// gate with this table means a new tool is one entry, and an unknown name is a
// clean instructive error rather than a missed branch. The OpenRouter server
// tool is deliberately absent — it resolves upstream and never reaches Run.
var toolHandlers = map[string]func(*BotToolbox, context.Context, json.RawMessage) (toolResult, error){
	memoryToolName:    (*BotToolbox).runMemory,
	calendarToolName:  (*BotToolbox).runCalendar,
	webSearchFuncName: (*BotToolbox).runWebSearch,
}

// Run executes one tool call and returns its result. A malformed call returns an
// instructive "error: ..." RESULT (nil error) so the model can self-correct on
// the next iteration; a returned error is a real failure and fails the whole
// turn — the message strands as failed with the reason, retryable like any other
// failed turn. ctx is threaded through because web_search makes a network call;
// the memory handler ignores it.
func (tb *BotToolbox) Run(ctx context.Context, name string, args json.RawMessage) (toolResult, error) {
	handler, ok := toolHandlers[name]
	if !ok {
		return toolResult{text: fmt.Sprintf("error: unknown tool '%s' — valid: %s", name, strings.Join(toolNames(), ", "))}, nil
	}
	return handler(tb, ctx, args)
}

// toolNames lists the dispatchable tool names, for the unknown-tool error.
func toolNames() []string {
	return []string{memoryToolName, calendarToolName, webSearchFuncName}
}

// runMemory dispatches the memory tool through the memoryCommands registry. It
// ignores ctx — memory writes are local store operations.
func (tb *BotToolbox) runMemory(_ context.Context, args json.RawMessage) (toolResult, error) {
	var in struct {
		Command string  `json:"command"`
		Content *string `json:"content"` // pointer: absent and "" are different answers
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolResult{text: `error: arguments must be a JSON object like {"command": "read"}`}, nil
		}
	}
	if in.Command == "" {
		return toolResult{text: fmt.Sprintf("error: missing 'command' — valid: %s", strings.Join(commandNames(), ", "))}, nil
	}
	for _, c := range memoryCommands {
		if c.name != in.Command {
			continue
		}
		if c.needsContent && in.Content == nil {
			return toolResult{text: fmt.Sprintf("error: '%s' requires a 'content' field", c.name)}, nil
		}
		if !c.needsContent && in.Content != nil {
			return toolResult{text: fmt.Sprintf("error: '%s' takes no 'content' field — use 'replace' to overwrite your memory", c.name)}, nil
		}
		var content string
		if in.Content != nil {
			content = *in.Content
		}
		text, err := c.run(tb.store, tb.botID, content)
		return toolResult{text: text}, err
	}
	return toolResult{text: fmt.Sprintf("error: unknown command '%s' — valid: %s", in.Command, strings.Join(commandNames(), ", "))}, nil
}

// runCalendar dispatches the calendar tool through the calendarCommands
// registry — the same shape runMemory has, with the requirement check driven by
// each command's `requires` list rather than a per-command branch. It ignores
// ctx: every command is a local store operation.
func (tb *BotToolbox) runCalendar(_ context.Context, args json.RawMessage) (toolResult, error) {
	var in calendarArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolResult{text: `error: arguments must be a JSON object like {"command": "list"}`}, nil
		}
	}
	if in.Command == "" {
		return toolResult{text: fmt.Sprintf("error: missing 'command' — valid: %s",
			strings.Join(calendarCommandNames(), ", "))}, nil
	}
	for _, c := range calendarCommands {
		if c.name != in.Command {
			continue
		}
		for _, need := range c.requires {
			if _, ok := in.field(need); !ok {
				return toolResult{text: fmt.Sprintf("error: '%s' requires a '%s' field", c.name, need)}, nil
			}
		}
		text, err := c.run(tb.store, tb.botID, in)
		return toolResult{text: text}, err
	}
	return toolResult{text: fmt.Sprintf("error: unknown command '%s' — valid: %s",
		in.Command, strings.Join(calendarCommandNames(), ", "))}, nil
}

// runWebSearch dispatches the web_search tool: it runs the model's query through
// the router's active backend, renders a compact text list back to the model,
// and carries the structured sources out for the audit record and the reply's
// citations. A malformed call or a backend failure is an instructive error
// RESULT — the model can answer without search rather than failing the turn —
// and the backend name is recorded either way. Only offered when a backend is
// available, so tb.search.Active() is non-nil here.
func (tb *BotToolbox) runWebSearch(ctx context.Context, args json.RawMessage) (toolResult, error) {
	var in struct {
		Query      string `json:"query"`
		NumResults int    `json:"num_results"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolResult{text: `error: arguments must be a JSON object like {"query": "..."}`}, nil
		}
	}
	if strings.TrimSpace(in.Query) == "" {
		return toolResult{text: "error: missing 'query' — provide a search query string"}, nil
	}
	backend := tb.search.Active()
	resp, err := backend.Search(ctx, in.Query, SearchOpts{NumResults: in.NumResults})
	if err != nil {
		// Fail soft: a transient search failure answers the model with an
		// instructive error it can proceed past, and the call is still audited
		// (backend named, no results) rather than failing the turn.
		return toolResult{text: fmt.Sprintf("error: web search failed: %v", err), backend: backend.Name()}, nil
	}
	cites := make([]Citation, 0, len(resp.Results))
	for _, r := range resp.Results {
		cites = append(cites, Citation{URL: r.URL, Title: r.Title, Snippet: r.Snippet})
	}
	return toolResult{
		text:      renderSearchResults(in.Query, backend.Name(), resp.Results),
		backend:   backend.Name(),
		requestID: resp.RequestID,
		results:   cites,
	}, nil
}

// renderSearchResults formats the backend's results as the compact numbered
// list fed back to the model — enough to read and cite, not a wall of text.
func renderSearchResults(query, backend string, results []SearchResult) string {
	if len(results) == 0 {
		return fmt.Sprintf("No web results found for %q (via %s).", query, backend)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Web results for %q (via %s):\n", query, backend)
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s — %s\n", i+1, r.Title, r.URL)
		if r.PublishedAt != "" {
			fmt.Fprintf(&b, "   (%s)\n", r.PublishedAt)
		}
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
