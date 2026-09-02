package botnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
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

	// The firing surface (see the Event DECISIONs in schema.go). Executable
	// is a string "true"/"false" rather than a JSON bool because every field
	// of this flat schema is a string — mid-tier models mix types up.
	RRule      *string `json:"rrule"`
	TZ         *string `json:"tz"`
	Automation *string `json:"automation"`
	Executable *string `json:"executable"`
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
	case "rrule":
		p = a.RRule
	case "tz":
		p = a.TZ
	case "automation":
		p = a.Automation
	case "executable":
		p = a.Executable
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
			`(a calendar name; defaults to "Personal"). Optional "rrule" makes it recur ` +
			`(RFC 5545, e.g. FREQ=MONTHLY;BYDAY=4TU — requires "tz", an IANA zone like ` +
			`America/New_York, so the wall-clock time survives DST); optional "automation" ` +
			`names an automation the event fires while active — allowed only on an ` +
			`executable calendar.`,
		run: runCalendarCreate,
	},
	{
		name: "list",
		doc: `"list": shows the calendar. Optional "from" and "to" bound the window; ` +
			`it defaults to the next 14 days. Optional "calendar" (a calendar name) shows ` +
			`only that calendar. Recurring events appear once per occurrence in the window, ` +
			`annotated with their rule. The first line is always the current time, ` +
			`so use it to resolve "today", "tomorrow" and "next week".`,
		run: runCalendarList,
	},
	{
		name:     "update",
		requires: []string{"event_id"},
		doc: `"update": changes an existing event. Requires "event_id" (from "list") plus ` +
			`any of "title", "start", "end", "location", "notes", "calendar" (moves it to ` +
			`that calendar), "rrule", "tz", "automation" — omitted fields are left alone.`,
		run: runCalendarUpdate,
	},
	{
		name:     "delete",
		requires: []string{"event_id"},
		doc: `"delete": cancels an event. Requires "event_id" (from "list"). Deleting a ` +
			`recurring event removes its whole series.`,
		run: runCalendarDelete,
	},
	{
		name:     "create_calendar",
		requires: []string{"name"},
		doc: `"create_calendar": adds a named calendar. Requires "name"; optional "color" ` +
			`(one of ` + strings.Join(calendarColors, ", ") + ` — assigned automatically when omitted) ` +
			`and "executable" ("true" or "false") — only an executable calendar's events may ` +
			`fire automations.`,
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
		doc: `"rename_calendar": renames, recolors or re-flags a calendar. Requires "calendar" ` +
			`(its current name) plus any of "name" (the new name), "color", "executable" ` +
			`("true" or "false").`,
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
				"event_id":   str(`The event to change, for "update" and "delete". Take it from "list".`),
				"title":      str(`The event's title, for "create" and "update".`),
				"start":      str(`When the event starts, RFC3339, for "create" and "update".`),
				"end":        str(`When the event ends, RFC3339. Optional; "create" defaults to one hour after the start.`),
				"location":   str(`Where it happens. Optional.`),
				"notes":      str(`Anything else worth keeping on the event. Optional.`),
				"from":       str(`Start of the window to list, RFC3339. Optional; defaults to now.`),
				"to":         str(`End of the window to list, RFC3339. Optional; defaults to 14 days out.`),
				"calendar":   str(`A calendar, by name (case-insensitive). Optional filter/target for "create", "update" and "list"; the current name for "rename_calendar" and "delete_calendar".`),
				"name":       str(`A calendar's name: the new calendar for "create_calendar", the new name for "rename_calendar".`),
				"color":      str(`A calendar color, one of ` + strings.Join(calendarColors, ", ") + `, for "create_calendar" and "rename_calendar". Optional.`),
				"rrule":      str(`An RFC 5545 recurrence rule making the event repeat, e.g. FREQ=MONTHLY;BYDAY=4TU (supported: FREQ, INTERVAL, COUNT, UNTIL, BYDAY, BYMONTHDAY, BYMONTH, BYSETPOS, WKST). Requires "tz". Optional, for "create" and "update".`),
				"tz":         str(`The IANA zone the recurrence's wall clock lives in, e.g. America/New_York. Required with "rrule".`),
				"automation": str(`The automation this event fires while an instance is active. Only allowed on an executable calendar. Optional, for "create" and "update".`),
				"executable": str(`"true" or "false": whether the calendar's events may fire automations. Optional, for "create_calendar" and "rename_calendar".`),
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
	rrule, _ := a.field("rrule")
	tz, _ := a.field("tz")
	automation, _ := a.field("automation")
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
	// name an author, so an event always says who really booked it. The
	// recurrence rules (rrule needs tz, automation needs an executable
	// calendar) live in the store and come back as instructive errors.
	ev, err := s.CreateEvent(Event{
		Title: title, StartsAt: start, EndsAt: end, Location: location, Notes: notes,
		CalendarID: calID, RRule: rrule, TZ: tz, Automation: automation,
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
	// The window operates on INSTANCES: a recurring event appears once per
	// occurrence, exactly what the model needs to reason about "this week".
	instances, err := s.Instances(from, to)
	if err != nil {
		return calendarStoreError(err)
	}
	if only != nil {
		kept := instances[:0]
		for _, in := range instances {
			if in.CalendarID == only.ID {
				kept = append(kept, in)
			}
		}
		instances = kept
	}
	// Annotate each instance with its calendar's name whenever there is more
	// than one calendar to tell apart — a single-calendar net reads as before —
	// and with its master's rule, so the schedule is legible and editable.
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
	rules := map[EventID]string{}
	for _, in := range instances {
		if in.Recurring {
			rules[in.EventID] = "" // fill below, one fetch per distinct master
		}
	}
	for id := range rules {
		master, err := s.GetEvent(id)
		if err != nil {
			return "", err
		}
		rules[id] = master.RRule
	}
	return renderCalendar(now, from, to, instances, names, rules), nil
}

// executableArg reads the optional "executable" field: value, whether it was
// supplied, and the instructive error for anything but "true"/"false". Every
// field of the flat schema is a string, so the boolean is spelled out.
func executableArg(a calendarArgs) (value, supplied bool, errText string) {
	raw, ok := a.field("executable")
	if !ok {
		return false, false, ""
	}
	switch strings.ToLower(raw) {
	case "true":
		return true, true, ""
	case "false":
		return false, true, ""
	}
	return false, false, fmt.Sprintf(`error: 'executable' must be "true" or "false", not %q`, raw)
}

func runCalendarCreateCalendar(s *Store, botID BotID, a calendarArgs) (string, error) {
	name, _ := a.field("name")
	color, _ := a.field("color")
	executable, _, errText := executableArg(a)
	if errText != "" {
		return errText, nil
	}
	// createdBy is the calling bot, exactly as event creates stamp it.
	cal, err := s.CreateCalendar(name, color, string(botID), executable)
	if err != nil {
		return calendarStoreError(err)
	}
	desc := cal.Color
	if cal.Executable {
		desc += ", executable"
	}
	return fmt.Sprintf("created calendar %q (%s)", cal.Name, desc), nil
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
	executable, supplied, errText := executableArg(a)
	if errText != "" {
		return errText, nil
	}
	if supplied {
		p.Executable = &executable
	}
	if p.Name == nil && p.Color == nil && p.Executable == nil {
		return "error: 'rename_calendar' needs a 'name' (the new name), a 'color' or an 'executable' to change", nil
	}
	updated, err := s.UpdateCalendar(cal.ID, p)
	if err != nil {
		return calendarStoreError(err)
	}
	desc := updated.Color
	if updated.Executable {
		desc += ", executable"
	}
	return fmt.Sprintf("updated calendar %q (%s)", updated.Name, desc), nil
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
		{"rrule", func(v string) { p.RRule = &v }},
		{"tz", func(v string) { p.TZ = &v }},
		{"automation", func(v string) { p.Automation = &v }},
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
		return "error: 'update' needs at least one of title, start, end, location, notes, calendar, rrule, tz, automation to change", nil
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
// compact numbered line per INSTANCE — a recurring event appears once per
// occurrence, its line carrying the master's id (what update/delete take) and
// its rule. The now line is what makes the tool usable at all — without it
// "book lunch tomorrow" has no anchor. A non-nil names map annotates each
// instance with its calendar — the caller passes one exactly when more than
// one calendar exists, so a single-calendar net stays uncluttered; rules maps
// a recurring master to its RRULE for the (repeats: ...) annotation.
func renderCalendar(now, from, to time.Time, instances []Instance, names map[CalendarID]string, rules map[EventID]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "now: %s\n", localRFC3339(now))
	if len(instances) == 0 {
		fmt.Fprintf(&b, "(no events between %s and %s)", localRFC3339(from), localRFC3339(to))
		return b.String()
	}
	fmt.Fprintf(&b, "%d event(s) between %s and %s:\n", len(instances), localRFC3339(from), localRFC3339(to))
	for i, in := range instances {
		fmt.Fprintf(&b, "%d. %s  %s → %s  %s", i+1, in.EventID,
			localRFC3339(in.StartsAt), localRFC3339(in.EndsAt), in.Title)
		if in.Recurring && rules[in.EventID] != "" {
			fmt.Fprintf(&b, "  (repeats: %s)", rules[in.EventID])
		}
		if in.Automation != "" {
			fmt.Fprintf(&b, "  (fires: %s)", in.Automation)
		}
		if name, ok := names[in.CalendarID]; ok {
			fmt.Fprintf(&b, "  [%s]", name)
		}
		if in.Location != "" {
			fmt.Fprintf(&b, "  @ %s", in.Location)
		}
		fmt.Fprintf(&b, "  (by %s)\n", in.CreatedBy)
		if in.Notes != "" {
			fmt.Fprintf(&b, "   %s\n", in.Notes)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ── project ───────────────────────────────────────────────────────────────────
// The bot's half of the projects service, in the same command-registry shape
// the memory and calendar tools use. projectCommands is THE registry — the
// enum, the advertised description and the dispatch all derive from it.
//
// DECISION (bots address projects and facts by NAME): there is no id anywhere
// in this surface. A model that has to carry a prj_ id between turns loses it;
// a project name is what the user said out loud, and a fact is resolved by its
// title within its project. The cost is the ambiguity error below, which is
// cheap and instructive.
//
// DECISION (no delete commands): deletion is UI-only, behind a confirmation —
// the delete_calendar precedent. A cheap model must not be able to drop a
// project's whole history, or tick a fact off by removing it.

// projectToolName is the tool the model calls to read and write projects.
const projectToolName = "project"

// projectLadder is the decision procedure a bot follows to file something, and
// it is the SOURCE OF TRUTH for how this tool is used — PROJECTS.md quotes it,
// never the other way round.
//
// DECISION (encode the usage pattern, do not hope for it): bots operate
// projects, not the user, and a mid-tier model asked to "record this" defaults
// to a note, which is the one kind that changes nothing. A numbered ladder in
// the description costs a few hundred tokens per turn and removes the whole
// class of judgment calls; the guards below then make the four known misuses
// instructive errors rather than silent drift, so the ladder is enforced and
// not merely advertised.
const projectLadder = `How to record something — take the FIRST rule that fits:
1. A date in the future you must act by → kind=deadline with "due". The lead window defaults from the PROJECT, so set it once with "default_lead_days" on the project that holds this kind of date (passport renewals 180, visa renewals 90, company filings 60; anything else 30) rather than typing "lead_days" into every fact; "lead_days" is for the one fact that differs.
2. An obligation that repeats → kind=recurring with "due" (the FIRST occurrence), "rrule" and "tz".
3. A step someone must complete → kind=milestone. If a HUMAN must act, set "blocker" to exactly what they must do, and clear it ("blocker": "") once they have. A blocked step cannot also be done.
4. Only "what happened" or "what I learned" → kind=note. A note NEVER changes health, so if you are about to write a date into one, it is a deadline: go back to 1.
5. Before "create" or "add_fact", run "show" on the project. Update the existing fact with update_fact rather than adding a twin — a duplicate title is refused.
6. When a deadline is renewed (a new passport arrives), set "due" to the new date. Mark it done ONLY when the obligation itself no longer exists.
7. Never mark anything done without evidence, and record that evidence as a note in the same turn.
8. A document or entity with its own dates and notes under a bigger goal (Passport under Document Expirations) → a sub-project: "create" with "parent". A single date under a project → a fact.`

// noteTitleLimit caps the title the "note" shorthand derives from the body: a
// note is a paragraph, and its title is just the handle the list shows.
const noteTitleLimit = 60

// projectArgs is the flat argument object every command shares — flat strings
// for the same reason calendarArgs is, including the booleans and the day
// count, since a type mix is exactly what mid-tier models fumble. The fields
// are POINTERS so an absent one and an explicitly empty one are different
// answers: clearing a blocker is "blocker": "", which must not read the same as
// leaving it alone.
type projectArgs struct {
	Command         string  `json:"command"`
	Project         *string `json:"project"`
	Parent          *string `json:"parent"`
	NewName         *string `json:"new_name"`
	Goal            *string `json:"goal"`
	DefaultLeadDays *string `json:"default_lead_days"`
	Owner           *string `json:"owner"`
	Kind            *string `json:"kind"`
	Title           *string `json:"title"`
	NewTitle        *string `json:"new_title"`
	Due             *string `json:"due"`
	LeadDays        *string `json:"lead_days"`
	RRule           *string `json:"rrule"`
	TZ              *string `json:"tz"`
	Done            *string `json:"done"`
	Blocker         *string `json:"blocker"`
	Body            *string `json:"body"`
}

// ptr resolves one flat field by its wire name — the single switch the two
// accessors below share, so `requires` can be a list of names.
func (a projectArgs) ptr(name string) *string {
	switch name {
	case "project":
		return a.Project
	case "parent":
		return a.Parent
	case "new_name":
		return a.NewName
	case "goal":
		return a.Goal
	case "default_lead_days":
		return a.DefaultLeadDays
	case "owner":
		return a.Owner
	case "kind":
		return a.Kind
	case "title":
		return a.Title
	case "new_title":
		return a.NewTitle
	case "due":
		return a.Due
	case "lead_days":
		return a.LeadDays
	case "rrule":
		return a.RRule
	case "tz":
		return a.TZ
	case "done":
		return a.Done
	case "blocker":
		return a.Blocker
	case "body":
		return a.Body
	}
	return nil
}

// field reports a field supplied with a NON-EMPTY value — what a requirement
// check means.
func (a projectArgs) field(name string) (string, bool) {
	p := a.ptr(name)
	if p == nil || *p == "" {
		return "", false
	}
	return *p, true
}

// given reports a field the model SUPPLIED, empty or not — what a patch means,
// so "blocker": "" clears the blocker instead of reading as absent.
func (a projectArgs) given(name string) (string, bool) {
	p := a.ptr(name)
	if p == nil {
		return "", false
	}
	return *p, true
}

// projectCommand declares one command of the project tool.
type projectCommand struct {
	name     string
	requires []string // flat fields that must be present and non-empty
	doc      string   // the description line advertised to the model
	run      func(s *Store, botID BotID, a projectArgs) (string, error)
}

// projectCommands is the registry — the single place a command is declared.
var projectCommands = []projectCommand{
	{
		name: "list",
		doc: `"list": shows every project with its derived health (` +
			`overdue, blocked, due_soon, ok, unknown), when it is next due and how many facts it holds.`,
		run: runProjectList,
	},
	{
		name:     "show",
		requires: []string{"project"},
		doc: `"show": lists one project's facts, most urgent first. Requires "project" ` +
			`(its name, case-insensitive).`,
		run: runProjectShow,
	},
	{
		name:     "create",
		requires: []string{"project"},
		doc: `"create": starts a project. Requires "project" (its name); optional "goal", ` +
			`"parent" (an existing project's name, making this one a sub-project of it), ` +
			`"default_lead_days" (the lead window every dated fact here takes unless it names its own) ` +
			`and "owner" (the bot answerable for it, by display name).`,
		run: runProjectCreate,
	},
	{
		name:     "update",
		requires: []string{"project"},
		doc: `"update": changes a project itself, not its facts. Requires "project" plus any of ` +
			`"new_name", "goal", "parent" (an existing project's name to move it under, or "none" to ` +
			`make it top-level), "default_lead_days" ("0" clears it and the parent's applies again), ` +
			`"owner" (a bot's display name, or "none" to clear it) — omitted fields are left alone.`,
		run: runProjectUpdate,
	},
	{
		name:     "add_fact",
		requires: []string{"project", "kind", "title"},
		doc: `"add_fact": records something true about a project. Requires "project", "title" and ` +
			`"kind", one of: "deadline" (one dated obligation — requires "due"), "recurring" (a dated ` +
			`obligation that repeats — requires "due" for the FIRST occurrence plus "rrule" and "tz"), ` +
			`"milestone" (a step, optionally "blocker" naming what human action it waits on), "note" ` +
			`(undated context in "body"). Optional "lead_days" is how many days before the due date it ` +
			`counts as due soon — omit it and the fact takes the project's own window — and "body" ` +
			`holds details for any kind.`,
		run: runProjectAddFact,
	},
	{
		name:     "update_fact",
		requires: []string{"project", "title"},
		doc: `"update_fact": changes a fact. Requires "project" and "title" (the fact's current ` +
			`title, case-insensitive) plus any of "new_title", "due", "lead_days", "rrule" and "tz" ` +
			`(a recurring fact's rule — changing either moves every future occurrence), "done" ("true" ` +
			`or "false"), "blocker" (empty clears it), "body" — omitted fields are left alone.`,
		run: runProjectUpdateFact,
	},
	{
		name:     "note",
		requires: []string{"project", "body"},
		doc: `"note": shorthand for adding a note fact. Requires "project" and "body"; the title is ` +
			`taken from the start of the body.`,
		run: runProjectNote,
	},
}

// projectCommandNames lists the enum, in registry order.
func projectCommandNames() []string {
	names := make([]string, len(projectCommands))
	for i, c := range projectCommands {
		names[i] = c.name
	}
	return names
}

// projectToolDef renders the registry as the wire tool definition, the same way
// memoryToolDef and calendarToolDef do: prose per command, one strict enum,
// flat strings.
func projectToolDef() wireTool {
	lines := []string{
		"Read and write the shared projects: what work is ABOUT, as against the calendar's " +
			"appointments. A project is a goal plus typed, dated FACTS (a passport expiry, a company " +
			"formation's milestones, a recurring annual filing), and its health is derived from those " +
			"facts — you never set it. You, the user and the other bots all see the same projects. " +
			"Dates are RFC3339 (e.g. 2027-03-14T00:00:00Z).",
		projectLadder,
		"Commands:",
	}
	for _, c := range projectCommands {
		lines = append(lines, "- "+c.doc)
	}
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	return wireTool{Type: "function", Function: wireToolFunction{
		Name:        projectToolName,
		Description: strings.Join(lines, "\n"),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"enum":        projectCommandNames(),
					"description": "The operation to perform.",
				},
				"project":  str(`A project, by name (case-insensitive). Required by every command but "list".`),
				"parent":   str(`The project this one sits under, by name. Optional, for "create" and "update"; "none" makes it top-level again.`),
				"new_name": str(`A project's new name, for "update". Optional.`),
				"goal":     str(`What the project is for, one line. Optional, for "create" and "update".`),
				"default_lead_days": str(`How many days before a due date the facts of THIS project count as ` +
					`due soon, e.g. "180". Optional, for "create" and "update"; sub-projects inherit it, and "0" ` +
					`clears it so the parent's applies again.`),
				"owner": str(`The bot answerable for this project, by DISPLAY NAME (case-insensitive) — it is ` +
					`the one nudged when the project gets worse. Optional, for "create" and "update"; ` +
					`sub-projects inherit it, and "none" clears it so the parent's applies again.`),
				"kind":      str(`The kind of fact: ` + strings.Join(factKinds(), ", ") + `. Required for "add_fact".`),
				"title":     str(`The fact's title for "add_fact"; the fact to change for "update_fact".`),
				"new_title": str(`A fact's new title, for "update_fact". Optional.`),
				"due":       str(`When the fact is due, RFC3339. Required for a deadline; the FIRST occurrence for a recurring fact.`),
				"lead_days": str(`How many days before the due date THIS ONE fact counts as due soon, e.g. "180". Optional; omitted, it takes the project's window.`),
				"rrule":     str(`An RFC 5545 recurrence rule for a recurring fact, e.g. FREQ=YEARLY (supported: FREQ, INTERVAL, COUNT, UNTIL, BYDAY, BYMONTHDAY, BYMONTH, BYSETPOS, WKST). Requires "tz". Settable on "update_fact" too, which moves every future occurrence.`),
				"tz":        str(`The IANA zone the recurrence's wall clock lives in, e.g. Asia/Singapore. Required with "rrule".`),
				"done":      str(`"true" or "false": whether a deadline or milestone is finished. Optional, for "update_fact".`),
				"blocker":   str(`What human action a milestone is waiting on; an empty value clears it. Optional.`),
				"body":      str(`The note's text, or details for any other kind.`),
			},
			"required": []string{"command"},
		},
	}}
}

// projectNamed resolves the "project" name arg. An unknown name answers with an
// instructive error listing the projects that DO exist — deliberately not
// auto-creating, because a typo must not spawn a project. The three returns are
// the tool-handler split: a project, an instructive error text for the model, or
// a real store failure.
func projectNamed(s *Store, name string) (Project, string, error) {
	p, err := s.ProjectByName(name)
	if err == nil {
		return p, "", nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Project{}, "", err
	}
	all, err := s.ListProjects()
	if err != nil {
		return Project{}, "", err
	}
	if len(all) == 0 {
		return Project{}, fmt.Sprintf("error: no project named %q — none exist yet; create makes one", name), nil
	}
	names := make([]string, len(all))
	for i, p := range all {
		names[i] = p.Name
	}
	return Project{}, fmt.Sprintf("error: no project named %q — existing projects: %s",
		name, strings.Join(names, ", ")), nil
}

// factNamed resolves a fact by title within a project, case-insensitively.
// Ambiguity is reported rather than guessed: picking one of two same-titled
// facts would silently edit the wrong one.
func factNamed(s *Store, p Project, title string) (Fact, string, error) {
	facts, err := s.ListFacts(p.ID)
	if err != nil {
		return Fact{}, "", err
	}
	var matches []Fact
	for _, f := range facts {
		if strings.EqualFold(strings.TrimSpace(f.Title), strings.TrimSpace(title)) {
			matches = append(matches, f)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], "", nil
	case 0:
		return Fact{}, fmt.Sprintf("error: no fact titled %q in %q — call show to see its facts", title, p.Name), nil
	}
	return Fact{}, fmt.Sprintf("error: %q matches %d facts in %q — rename one first so the fact you mean is unambiguous",
		title, len(matches), p.Name), nil
}

// boolArg reads one "true"/"false" flat field: value, whether it was supplied,
// and the instructive error for anything else — the executableArg shape, for
// the same reason.
func boolArg(a projectArgs, name string) (value, supplied bool, errText string) {
	raw, ok := a.field(name)
	if !ok {
		return false, false, ""
	}
	switch strings.ToLower(raw) {
	case "true":
		return true, true, ""
	case "false":
		return false, true, ""
	}
	return false, false, fmt.Sprintf(`error: '%s' must be "true" or "false", not %q`, name, raw)
}

// daysArg reads one optional day-count field ("lead_days" on a fact,
// "default_lead_days" on a project) as a whole number of days. One function
// rather than two, so the two windows cannot end up parsing or refusing
// differently — and "0" is a real supplied value, which is how a model clears a
// project's own threshold.
func daysArg(a projectArgs, name string) (value int, supplied bool, errText string) {
	raw, ok := a.field(name)
	if !ok {
		return 0, false, ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0, false, fmt.Sprintf(`error: '%s' must be a whole number of days, e.g. "30", not %q`, name, raw)
	}
	return n, true, ""
}

// ── Guards and derived state ──────────────────────────────────────────────────
// The four known misuses, turned into instructive errors, plus the health line
// every mutating result ends with. Two of the guards live in the store (a
// duplicate title, a blocked-and-done milestone), so REST enforces them too;
// the two below are TOOL-BOUNDARY only, because the thing they catch is a model
// misfiling something, not a human meaning what they typed.

// isoDate matches a bare YYYY-MM-DD anywhere in a string. It is a candidate
// finder, not a validator: time.Parse decides whether the digits are a date, so
// "invoice 2027-99-45" is just a number.
var isoDate = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

// dateInText returns the first real calendar date in the text.
func dateInText(text string) (string, bool) {
	for _, candidate := range isoDate.FindAllString(text, -1) {
		if _, err := time.Parse(time.DateOnly, candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// refuseDatedNote is the guard behind ladder rule 4. A note never moves health,
// so a date filed as one is an obligation the project will never surface — and
// the model gets back the exact call it should have made instead. Returns "" to
// allow the write.
func refuseDatedNote(title, body string) string {
	for _, text := range []string{title, body} {
		date, ok := dateInText(text)
		if !ok {
			continue
		}
		return fmt.Sprintf("error: that note carries the date %s — a date you must act by is a deadline, "+
			`not a note; resend it as {"command": "add_fact", "kind": "deadline", "title": "...", "due": %q}`,
			date, date+"T00:00:00Z")
	}
	return ""
}

// duplicateTitleError renders the store's uniqueness refusal with the command
// that fixes it. The store deliberately does not name update_fact — it has no
// business knowing a tool's command names, and the same rejection is a plain 409
// on the REST path.
func duplicateTitleError(title, project string) string {
	return fmt.Sprintf("error: fact %q already exists in %s; use update_fact", title, project)
}

// wholeDays renders a span the way a person counts it: to the NEAREST day. A
// deadline exactly 200 days out is 199.999 days by the time the write lands, and
// "199 days before due" in the answer would look like an off-by-one bug.
func wholeDays(d time.Duration) int { return int(d.Round(24*time.Hour) / (24 * time.Hour)) }

// healthLine is the one-line state a mutating result ends with, e.g.
// `Passports: S1 due_soon, next due 2027-03-14 (in 193d), lead 180d`. The
// severity band leads, so a model reading one result knows how loud it is
// without holding a table of five healths — and the value is the same string
// the app colours from. The lead closes the line because it is the answer to
// "why is this amber and not green", and because it is the window the NEXT fact
// filed here will take, inherited or not.
func healthLine(now time.Time, p Project) string {
	lead := fmt.Sprintf(", lead %dd", p.EffectiveLeadDays)
	if p.NextDue == nil {
		return fmt.Sprintf("%s: %s %s%s", p.Name, p.Severity, p.Health, lead)
	}
	due := p.NextDue.Local()
	if due.Before(now) {
		return fmt.Sprintf("%s: %s %s, next due %s (%dd overdue)%s",
			p.Name, p.Severity, p.Health, due.Format(time.DateOnly), wholeDays(now.Sub(due)), lead)
	}
	return fmt.Sprintf("%s: %s %s, next due %s (in %dd)%s",
		p.Name, p.Severity, p.Health, due.Format(time.DateOnly), wholeDays(due.Sub(now)), lead)
}

// withHealth appends the project's CURRENT health line to a result. Health is
// derived, so the copy the handler was holding is already stale by the time it
// answers — re-reading is how the model sees what its own write did.
func withHealth(s *Store, id ProjectID, text string) (string, error) {
	p, _, err := s.GetProject(id)
	if err != nil {
		return "", err
	}
	return text + "\n" + healthLine(time.Now(), p), nil
}

func runProjectList(s *Store, _ BotID, _ projectArgs) (string, error) {
	projects, err := s.ListProjects()
	if err != nil {
		return "", err
	}
	if len(projects) == 0 {
		return "(no projects yet — 'create' starts one)", nil
	}
	return renderProjectTree(time.Now(), projects), nil
}

// renderProjectTree walks the server's FLAT, urgency-ordered array into the
// indented tree a model reads as a shape: children under their parent, two
// spaces per level, severity leading every line. The list is one call and the
// nesting is derived here rather than fetched, exactly as the app derives it.
//
// The walk starts from the roots in the server's order and recurses in it, so
// the tree is still "most urgent first" within each level; an orphaned pointer
// makes a root, matching what the store's own derivation does with one.
func renderProjectTree(now time.Time, projects []Project) string {
	known := map[ProjectID]bool{}
	for _, p := range projects {
		known[p.ID] = true
	}
	byParent := map[ProjectID][]Project{}
	var roots []Project
	for _, p := range projects {
		if p.ParentID != "" && known[p.ParentID] {
			byParent[p.ParentID] = append(byParent[p.ParentID], p)
			continue
		}
		roots = append(roots, p)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "now: %s\n", localRFC3339(now))
	fmt.Fprintf(&b, "%d project(s), most urgent first:\n", len(projects))
	drawn := map[ProjectID]bool{}
	var walk func(p Project, depth int)
	walk = func(p Project, depth int) {
		if drawn[p.ID] {
			return
		}
		drawn[p.ID] = true
		fmt.Fprintf(&b, "%s- %s %s — %s", strings.Repeat("  ", depth), p.Severity, p.Name, p.Health)
		if p.NextDue != nil {
			fmt.Fprintf(&b, ", next due %s", localRFC3339(*p.NextDue))
		}
		fmt.Fprintf(&b, "  (%d fact(s)", p.FactCount)
		if p.ChildCount > 0 {
			fmt.Fprintf(&b, ", %d sub-project(s)", p.ChildCount)
		}
		b.WriteString(")")
		if p.Goal != "" {
			fmt.Fprintf(&b, "  — %s", p.Goal)
		}
		b.WriteString("\n")
		for _, c := range byParent[p.ID] {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return strings.TrimRight(b.String(), "\n")
}

func runProjectShow(s *Store, _ BotID, a projectArgs) (string, error) {
	name, _ := a.field("project")
	p, errText, err := projectNamed(s, name)
	if err != nil || errText != "" {
		return errText, err
	}
	fresh, facts, children, err := s.GetProjectDetail(p.ID)
	if err != nil {
		return projectStoreError(err)
	}
	return renderProject(time.Now(), fresh, children, facts), nil
}

// healthBearing reports whether any fact can move the project's health at all.
// A project of nothing but notes derives nothing, and "ok" would read as a
// verdict rather than as the absence of one.
func healthBearing(facts []Fact) bool {
	for _, f := range facts {
		if f.Kind == FactDeadline || f.Kind == FactRecurring || f.Kind == FactMilestone {
			return true
		}
	}
	return false
}

// renderProject formats one project for the model: the current time first (the
// anchor "due in three weeks" needs), then the header, then its DIRECT
// sub-projects if it has any, then a line per fact in the store's urgency-first
// order.
//
// The sub-projects come before the facts because they are the reason the
// project's own health may not be explained by anything below: a parent showing
// S0 with three quiet facts is telling the model to look at a child. Only
// direct children are listed — a grandchild belongs on its own parent's page,
// and the model gets there by showing that one.
func renderProject(now time.Time, p Project, children []Project, facts []Fact) string {
	var b strings.Builder
	// The header names the effective lead for the same reason the health line
	// does: it is the window a fact added here will take, and a model that
	// cannot see it types one in by hand.
	fmt.Fprintf(&b, "now: %s\n%s — %s %s, lead %dd",
		localRFC3339(now), p.Name, p.Severity, p.Health, p.EffectiveLeadDays)
	if p.Goal != "" {
		fmt.Fprintf(&b, "\ngoal: %s", p.Goal)
	}
	if len(children) > 0 {
		b.WriteString("\nSub-projects:")
		for _, c := range children {
			fmt.Fprintf(&b, "\n- %s %s — %s", c.Severity, c.Name, c.Health)
			if c.NextDue != nil {
				fmt.Fprintf(&b, ", next due %s", localRFC3339(*c.NextDue))
			}
		}
	}
	if len(facts) == 0 {
		b.WriteString("\n(no facts yet — add_fact records one)")
		b.WriteString("\n" + noHealthPrompt)
		return b.String()
	}
	fmt.Fprintf(&b, "\n%d fact(s), most urgent first:\n", len(facts))
	for i, f := range facts {
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, f.Kind, f.Title)
		if !f.Due.IsZero() {
			fmt.Fprintf(&b, "  due %s (lead %dd)", localRFC3339(f.Due), f.LeadDays)
		}
		if f.RRule != "" {
			fmt.Fprintf(&b, "  (repeats: %s, %s)", f.RRule, f.TZ)
		}
		if f.Blocker != "" {
			fmt.Fprintf(&b, "  BLOCKED: %s", f.Blocker)
		}
		if f.Done {
			b.WriteString("  (done)")
		}
		b.WriteString("\n")
		if f.Body != "" {
			fmt.Fprintf(&b, "   %s\n", f.Body)
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	if !healthBearing(facts) {
		out += "\n" + noHealthPrompt
	}
	return out
}

// noHealthPrompt is what a project with nothing health-bearing ends with: the
// model is told WHY there is no verdict and which kinds would produce one.
const noHealthPrompt = "health unknown: add a deadline, recurring or milestone fact"

// noParentWord is what a model writes to make a project top-level again. A
// sentinel word rather than an empty string, because the flat schema's empty
// value already means "field not supplied" everywhere else in this tool, and a
// model that means "clear it" must be able to say so out loud.
const noParentWord = "none"

// parentArg resolves the "parent" name a model supplied into an id: "" when it
// named none or wrote the sentinel, otherwise the named project, with the same
// instructive error projectNamed gives for any unknown name. It reports whether
// the field was SUPPLIED, so an update that never mentions a parent leaves the
// one a project has.
func parentArg(s *Store, a projectArgs) (id ProjectID, supplied bool, errText string, err error) {
	raw, ok := a.given("parent")
	if !ok {
		return "", false, "", nil
	}
	if strings.TrimSpace(raw) == "" || strings.EqualFold(strings.TrimSpace(raw), noParentWord) {
		return "", true, "", nil
	}
	parent, errText, err := projectNamed(s, raw)
	if err != nil || errText != "" {
		return "", true, errText, err
	}
	return parent.ID, true, "", nil
}

// ownerArg resolves the "owner" a model supplied into a BotID. Bots are named
// the way projects are — by the DISPLAY NAME a person says out loud, never by a
// bot_ id a model would have to carry between turns — and the sentinel that
// clears it is the same "none" the parent uses.
//
// Neither of the two non-answers is guessed: an unknown name lists the bots that
// exist, and two bots sharing one name is an ambiguity error, because picking
// one would silently make the wrong thread answerable for the project.
func ownerArg(s *Store, a projectArgs) (id BotID, supplied bool, errText string, err error) {
	raw, ok := a.given("owner")
	if !ok {
		return "", false, "", nil
	}
	name := strings.TrimSpace(raw)
	if name == "" || strings.EqualFold(name, noParentWord) {
		return "", true, "", nil
	}
	bots, err := s.AllBots()
	if err != nil {
		return "", true, "", err
	}
	var matches []Bot
	for _, b := range bots {
		if strings.EqualFold(strings.TrimSpace(b.DisplayName), name) {
			matches = append(matches, b)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, true, "", nil
	case 0:
		if len(bots) == 0 {
			return "", true, fmt.Sprintf("error: no bot named %q — there are no bots to own a project", name), nil
		}
		names := make([]string, len(bots))
		for i, b := range bots {
			names[i] = b.DisplayName
		}
		return "", true, fmt.Sprintf("error: no bot named %q — the bots are: %s",
			name, strings.Join(names, ", ")), nil
	}
	return "", true, fmt.Sprintf("error: %q matches %d bots — rename one so the owner you mean is unambiguous",
		name, len(matches)), nil
}

func runProjectCreate(s *Store, botID BotID, a projectArgs) (string, error) {
	name, _ := a.field("project")
	goal, _ := a.field("goal")
	parent, _, errText, err := parentArg(s, a)
	if err != nil || errText != "" {
		return errText, err
	}
	lead, _, errText := daysArg(a, "default_lead_days")
	if errText != "" {
		return errText, nil
	}
	owner, _, errText, err := ownerArg(s, a)
	if err != nil || errText != "" {
		return errText, err
	}
	// createdBy is the CALLING bot, stamped by the store — the model cannot
	// name an author, so a project always says who really started it.
	p, err := s.CreateProject(
		Project{Name: name, Goal: goal, ParentID: parent, DefaultLeadDays: lead, OwnerBot: owner},
		string(botID))
	if err != nil {
		return projectStoreError(err)
	}
	text := fmt.Sprintf("created project %q", p.Name)
	if p.ParentID != "" {
		if under, _, err := s.GetProject(p.ParentID); err == nil {
			text += fmt.Sprintf(" under %q", under.Name)
		}
	}
	return withHealth(s, p.ID, text)
}

// runProjectUpdate is the project's own patch — the counterpart to update_fact,
// and the only way a bot moves one project under another. There is still no
// delete: renaming and re-parenting are reversible, so they are safe for a
// cheap model in a way dropping a history is not.
func runProjectUpdate(s *Store, _ BotID, a projectArgs) (string, error) {
	name, _ := a.field("project")
	p, errText, err := projectNamed(s, name)
	if err != nil || errText != "" {
		return errText, err
	}
	var patch ProjectPatch
	// new_name is non-empty-only (a project must have a name); goal is
	// clearable, so it reads the SUPPLIED value, empty included.
	if v, ok := a.field("new_name"); ok {
		patch.Name = &v
	}
	if v, ok := a.given("goal"); ok {
		patch.Goal = &v
	}
	parent, supplied, errText, err := parentArg(s, a)
	if err != nil || errText != "" {
		return errText, err
	}
	if supplied {
		patch.ParentID = &parent
	}
	// "0" is a real value here: it clears this project's own threshold and lets
	// the nearest ancestor's apply again.
	if lead, given, errText := daysArg(a, "default_lead_days"); errText != "" {
		return errText, nil
	} else if given {
		patch.DefaultLeadDays = &lead
	}
	owner, given, errText, err := ownerArg(s, a)
	if err != nil || errText != "" {
		return errText, err
	}
	if given {
		patch.OwnerBot = &owner
	}
	if patch.Name == nil && patch.Goal == nil && patch.ParentID == nil &&
		patch.DefaultLeadDays == nil && patch.OwnerBot == nil {
		return "error: 'update' needs at least one of new_name, goal, parent, default_lead_days, owner to change", nil
	}
	updated, err := s.UpdateProject(p.ID, patch)
	if err != nil {
		return projectStoreError(err)
	}
	return withHealth(s, updated.ID, fmt.Sprintf("updated project %q", updated.Name))
}

func runProjectAddFact(s *Store, botID BotID, a projectArgs) (string, error) {
	name, _ := a.field("project")
	p, errText, err := projectNamed(s, name)
	if err != nil || errText != "" {
		return errText, err
	}
	kind, _ := a.field("kind")
	title, _ := a.field("title")
	f := Fact{Kind: FactKind(kind), Title: title}
	if raw, ok := a.field("due"); ok {
		due, err := calendarTime("due", raw)
		if err != nil {
			return err.Error(), nil
		}
		f.Due = due
	}
	lead, supplied, errText := daysArg(a, "lead_days")
	if errText != "" {
		return errText, nil
	}
	if supplied {
		f.LeadDays = lead
	}
	f.RRule, _ = a.field("rrule")
	f.TZ, _ = a.field("tz")
	f.Blocker, _ = a.field("blocker")
	f.Body, _ = a.field("body")
	if f.Kind == FactNote {
		if errText := refuseDatedNote(f.Title, f.Body); errText != "" {
			return errText, nil
		}
	}
	stored, err := s.CreateFact(p.ID, f, string(botID))
	if errors.Is(err, ErrDuplicateName) {
		return duplicateTitleError(f.Title, p.Name), nil
	}
	if err != nil {
		return projectStoreError(err)
	}
	return withHealth(s, p.ID, fmt.Sprintf("added %s to %q: %s", stored.Kind, p.Name, stored.Title))
}

// runProjectNote is add_fact's shorthand: the body IS the note, and the title
// is the handle a listing shows, taken from its start.
func runProjectNote(s *Store, botID BotID, a projectArgs) (string, error) {
	name, _ := a.field("project")
	p, errText, err := projectNamed(s, name)
	if err != nil || errText != "" {
		return errText, err
	}
	body, _ := a.field("body")
	title := noteTitle(body)
	if errText := refuseDatedNote(title, body); errText != "" {
		return errText, nil
	}
	stored, err := s.CreateFact(p.ID, Fact{Kind: FactNote, Title: title, Body: body}, string(botID))
	if errors.Is(err, ErrDuplicateName) {
		return duplicateTitleError(title, p.Name), nil
	}
	if err != nil {
		return projectStoreError(err)
	}
	return withHealth(s, p.ID, fmt.Sprintf("added note to %q: %s", p.Name, stored.Title))
}

// noteTitle takes the note's handle from the start of its body, counted in
// runes so a multi-byte body cannot be cut mid-character.
func noteTitle(body string) string {
	r := []rune(strings.Join(strings.Fields(body), " "))
	if len(r) > noteTitleLimit {
		return strings.TrimSpace(string(r[:noteTitleLimit]))
	}
	return string(r)
}

func runProjectUpdateFact(s *Store, _ BotID, a projectArgs) (string, error) {
	name, _ := a.field("project")
	p, errText, err := projectNamed(s, name)
	if err != nil || errText != "" {
		return errText, err
	}
	title, _ := a.field("title")
	f, errText, err := factNamed(s, p, title)
	if err != nil || errText != "" {
		return errText, err
	}
	var patch FactPatch
	changed := false
	// new_title is non-empty-only (a fact must have a title); blocker and body
	// are clearable, so they read the SUPPLIED value, empty included.
	if v, ok := a.field("new_title"); ok {
		patch.Title = &v
		changed = true
	}
	for _, field := range []struct {
		name string
		set  func(string)
	}{
		{"blocker", func(v string) { patch.Blocker = &v }},
		{"body", func(v string) { patch.Body = &v }},
		// rrule and tz belong to a recurring fact and go through the same
		// validation create does; changing either re-projects the fact's
		// calendar event inside UpdateFact's transaction, so the month grid
		// never keeps showing the rule the fact no longer has.
		{"rrule", func(v string) { patch.RRule = &v }},
		{"tz", func(v string) { patch.TZ = &v }},
	} {
		if v, ok := a.given(field.name); ok {
			field.set(v)
			changed = true
		}
	}
	if raw, ok := a.field("due"); ok {
		due, err := calendarTime("due", raw)
		if err != nil {
			return err.Error(), nil
		}
		patch.Due = &due
		changed = true
	}
	if lead, supplied, errText := daysArg(a, "lead_days"); errText != "" {
		return errText, nil
	} else if supplied {
		patch.LeadDays = &lead
		changed = true
	}
	if done, supplied, errText := boolArg(a, "done"); errText != "" {
		return errText, nil
	} else if supplied {
		patch.Done = &done
		changed = true
	}
	if !changed {
		return "error: 'update_fact' needs at least one of new_title, due, lead_days, rrule, tz, done, blocker, body to change", nil
	}
	updated, err := s.UpdateFact(f.ID, patch)
	if errors.Is(err, ErrDuplicateName) {
		intended := f.Title
		if patch.Title != nil {
			intended = *patch.Title
		}
		return duplicateTitleError(intended, p.Name), nil
	}
	if err != nil {
		return projectStoreError(err)
	}
	text := fmt.Sprintf("updated %q: %s", p.Name, updated.Title)
	// Ladder rule 6, as a caution rather than a refusal: ticking a deadline
	// months early is nearly always a renewal filed the wrong way, but the model
	// may be right, so the write stands and the answer names the alternative.
	now := time.Now()
	if patch.Done != nil && *patch.Done && updated.Kind == FactDeadline && updated.Due.After(now) {
		text += fmt.Sprintf("\nmarked done %d days before due; if this was a renewal, "+
			"set due to the new date instead", wholeDays(updated.Due.Sub(now)))
	}
	return withHealth(s, p.ID, text)
}

// projectStoreError splits the store's answer in two, exactly as
// calendarStoreError does: a missing row, a rejected value or a taken name is
// the MODEL's mistake and comes back as an instructive result it can fix on the
// next iteration; anything else is a real store failure and fails the turn.
func projectStoreError(err error) (string, error) {
	switch {
	case errors.Is(err, ErrNotFound):
		return "error: no such project or fact — call list to see what exists", nil
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrDuplicateName):
		return "error: " + storeErrorText(err), nil
	default:
		return "", err
	}
}

// storeErrorText strips the sentinel's own prefix, so the model reads the
// specific reason rather than the package's error taxonomy.
func storeErrorText(err error) string {
	text := err.Error()
	for _, prefix := range []string{"botnet: invalid: ", "botnet: that name is already taken: "} {
		text = strings.TrimPrefix(text, prefix)
	}
	return text
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
	defs := []any{memoryToolDef(), calendarToolDef(), projectToolDef()}
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
	projectToolName:   (*BotToolbox).runProject,
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
	return []string{memoryToolName, calendarToolName, projectToolName, webSearchFuncName}
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

// runProject dispatches the project tool through the projectCommands registry —
// the same shape runCalendar has, with the requirement check driven by each
// command's `requires` list rather than a per-command branch. It ignores ctx:
// every command is a local store operation.
func (tb *BotToolbox) runProject(_ context.Context, args json.RawMessage) (toolResult, error) {
	var in projectArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolResult{text: `error: arguments must be a JSON object like {"command": "list"}`}, nil
		}
	}
	if in.Command == "" {
		return toolResult{text: fmt.Sprintf("error: missing 'command' — valid: %s",
			strings.Join(projectCommandNames(), ", "))}, nil
	}
	for _, c := range projectCommands {
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
		in.Command, strings.Join(projectCommandNames(), ", "))}, nil
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
