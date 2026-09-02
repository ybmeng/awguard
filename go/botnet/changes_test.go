package botnet

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	modelselector "stdtools/go/lib/modelSelector"
)

// The DESIGN-sync.md appendix call-site table is the oracle these tests
// enforce: every mutating call site must produce exactly its expected change
// rows, via the schema's triggers rather than any Go code remembering to.

// changeRow is one raw change_log row, without its seq — the tests care what
// was emitted and in what order, not the absolute counter.
type changeRow struct{ entity, id, op string }

func topSeq(t *testing.T, s *Store) int64 {
	t.Helper()
	seq, err := s.maxSeq(s.db)
	if err != nil {
		t.Fatalf("max seq: %v", err)
	}
	return seq
}

// logAfter returns the raw change rows emitted after the given seq, in order.
func logAfter(t *testing.T, s *Store, after int64) []changeRow {
	t.Helper()
	rows, err := s.db.Query(
		`SELECT entity, entity_id, op FROM change_log WHERE seq > ? ORDER BY seq`, after)
	if err != nil {
		t.Fatalf("read change_log: %v", err)
	}
	defer rows.Close()
	var out []changeRow
	for rows.Next() {
		var r changeRow
		if err := rows.Scan(&r.entity, &r.id, &r.op); err != nil {
			t.Fatalf("scan change row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read change_log: %v", err)
	}
	return out
}

// expectRows asserts one call site's exact emission.
func expectRows(t *testing.T, got, want []changeRow, site string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s emitted %v, want %v", site, got, want)
	}
}

// TestEveryMutatingCallSiteEmitsItsChangeRows walks the store's mutating call
// sites and asserts each one's change rows, exactly as the design's call-site
// table specifies. The rows come from triggers, so this also proves the
// triggers exist and fire inside the same statements as the mutations.
func TestEveryMutatingCallSiteEmitsItsChangeRows(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	net, err := s.CreateNet("home")
	if err != nil {
		t.Fatalf("create net: %v", err)
	}

	// Nets are not a synced entity; creating one emits nothing.
	expectRows(t, logAfter(t, s, 0), nil, "CreateNet")

	mark := topSeq(t, s)
	bot, err := s.CreateBot(net.ID, "Ada", "prompt", modelselector.DeepSeekV4.ID)
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	seg0, err := s.OpenSegment(bot.ID)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"bot", string(bot.ID), "created"},
		{"segment", string(seg0.ID), "created"},
	}, "CreateBot")

	mark = topSeq(t, s)
	name := "Ada II"
	if _, err := s.UpdateBot(bot.ID, BotPatch{DisplayName: &name}, ""); err != nil {
		t.Fatalf("update bot: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{{"bot", string(bot.ID), "updated"}}, "UpdateBot")

	mark = topSeq(t, s)
	if _, err := s.MarkRead(bot.ID); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{{"bot", string(bot.ID), "updated"}}, "MarkRead")

	// SetMemory is the model's memory write path (the memory tool's replace
	// and clear commands). A memory-only UPDATE must still fire the bots
	// trigger, or a second client would never see the model take notes.
	mark = topSeq(t, s)
	if _, err := s.SetMemory(bot.ID, "remember this"); err != nil {
		t.Fatalf("set memory: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{{"bot", string(bot.ID), "updated"}}, "SetMemory")

	// AppendMessage is the easy one to get wrong: the message insert AND the
	// bot's denormalized list metadata both change, so it is two rows — the
	// sidebar depends on the second.
	mark = topSeq(t, s)
	msg, err := s.AppendMessage(bot.ID, "user", "hello", StatusSent)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"message", msg.ID, "created"},
		{"bot", string(bot.ID), "updated"},
	}, "AppendMessage")

	mark = topSeq(t, s)
	if err := s.SetMessageStatus(msg.ID, StatusFailed, "boom"); err != nil {
		t.Fatalf("set status: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{{"message", msg.ID, "updated"}}, "SetMessageStatus")

	mark = topSeq(t, s)
	failed, err := s.GetMessage(msg.ID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if err := s.ClaimRetry(failed); err != nil {
		t.Fatalf("claim retry: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{{"message", msg.ID, "updated"}}, "ClaimRetry")

	mark = topSeq(t, s)
	reply, err := s.CompleteTurn(bot.ID, msg.ID, "hi!", nil, nil)
	if err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"message", reply.ID, "created"},
		{"bot", string(bot.ID), "updated"},
		{"message", msg.ID, "updated"},
	}, "CompleteTurn")

	mark = topSeq(t, s)
	if err := s.Seal(seg0, "summary"); err != nil {
		t.Fatalf("seal: %v", err)
	}
	seg1, err := s.OpenSegment(bot.ID)
	if err != nil {
		t.Fatalf("open segment after seal: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"segment", string(seg0.ID), "updated"},
		{"segment", string(seg1.ID), "created"},
	}, "Seal")

	// The calendar partition. The Personal ensure is a real write the FIRST
	// time and a no-op ever after: the feed sees the birth exactly once, and a
	// repeat ensure must not wake clients with a phantom change.
	mark = topSeq(t, s)
	personal, err := s.EnsurePersonalCalendar()
	if err != nil {
		t.Fatalf("ensure personal calendar: %v", err)
	}
	expectRows(t, logAfter(t, s, mark),
		[]changeRow{{"calendar", string(personal.ID), "created"}}, "EnsurePersonalCalendar (first)")

	mark = topSeq(t, s)
	if _, err := s.EnsurePersonalCalendar(); err != nil {
		t.Fatalf("re-ensure personal calendar: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), nil, "EnsurePersonalCalendar (again)")

	mark = topSeq(t, s)
	earnings, err := s.CreateCalendar("Company Earnings", "", string(bot.ID), false)
	if err != nil {
		t.Fatalf("create calendar: %v", err)
	}
	expectRows(t, logAfter(t, s, mark),
		[]changeRow{{"calendar", string(earnings.ID), "created"}}, "CreateCalendar")

	mark = topSeq(t, s)
	teal := "teal"
	if _, err := s.UpdateCalendar(earnings.ID, CalendarPatch{Color: &teal}); err != nil {
		t.Fatalf("update calendar: %v", err)
	}
	expectRows(t, logAfter(t, s, mark),
		[]changeRow{{"calendar", string(earnings.ID), "updated"}}, "UpdateCalendar")

	// The calendar service. Events are owned by the net, not by the bot, so
	// they survive DeleteBot below — but each of the three writes must emit,
	// or a second client's Calendar panel goes stale.
	mark = topSeq(t, s)
	ev, err := s.CreateEvent(Event{
		Title:    "Lunch with Alex",
		StartsAt: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
		EndsAt:   time.Date(2026, 8, 31, 13, 0, 0, 0, time.UTC),
	}, string(bot.ID))
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{{"event", string(ev.ID), "created"}}, "CreateEvent")

	// A field-only UPDATE still fires the row trigger — the same property the
	// memory write above relies on.
	mark = topSeq(t, s)
	where := "the good taco place"
	if _, err := s.UpdateEvent(ev.ID, EventPatch{Location: &where}); err != nil {
		t.Fatalf("update event: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{{"event", string(ev.ID), "updated"}}, "UpdateEvent")

	mark = topSeq(t, s)
	if err := s.DeleteEvent(ev.ID); err != nil {
		t.Fatalf("delete event: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{{"event", string(ev.ID), "destroyed"}}, "DeleteEvent")

	// DeleteCalendar CASCADES: a real tombstone per event, then the calendar's
	// own — the explicit event DELETE is what fires the chg_event_* triggers,
	// so a sync client is never left holding events of a calendar it saw die.
	inEarnings, err := s.CreateEvent(Event{
		Title:      "Q3 earnings call",
		StartsAt:   time.Date(2026, 9, 15, 21, 0, 0, 0, time.UTC),
		EndsAt:     time.Date(2026, 9, 15, 22, 0, 0, 0, time.UTC),
		CalendarID: earnings.ID,
	}, string(bot.ID))
	if err != nil {
		t.Fatalf("create event in calendar: %v", err)
	}
	mark = topSeq(t, s)
	if err := s.DeleteCalendar(earnings.ID); err != nil {
		t.Fatalf("delete calendar: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"event", string(inEarnings.ID), "destroyed"},
		{"calendar", string(earnings.ID), "destroyed"},
	}, "DeleteCalendar")

	// The projects service. Health is derived and never stored, so nothing here
	// emits on a read — only the two authored entities do. An undated fact is
	// used deliberately: a dated one also writes its projected event, which
	// TestProjectedFactEmitsItsChangeRows covers.
	mark = topSeq(t, s)
	project, err := s.CreateProject(Project{Name: "Passports", Goal: "keep every passport valid"}, string(bot.ID))
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	expectRows(t, logAfter(t, s, mark),
		[]changeRow{{"project", string(project.ID), "created"}}, "CreateProject")

	mark = topSeq(t, s)
	goal := "renew before every trip"
	if _, err := s.UpdateProject(project.ID, ProjectPatch{Goal: &goal}); err != nil {
		t.Fatalf("update project: %v", err)
	}
	expectRows(t, logAfter(t, s, mark),
		[]changeRow{{"project", string(project.ID), "updated"}}, "UpdateProject")

	mark = topSeq(t, s)
	milestone, err := s.CreateFact(project.ID,
		Fact{Kind: FactMilestone, Title: "book the appointment"}, string(bot.ID))
	if err != nil {
		t.Fatalf("create fact: %v", err)
	}
	expectRows(t, logAfter(t, s, mark),
		[]changeRow{{"fact", string(milestone.ID), "created"}}, "CreateFact")

	// A field-only UPDATE still fires the row trigger, the same property the
	// memory and event writes above rely on.
	mark = topSeq(t, s)
	blocker := "waiting on the consulate"
	if _, err := s.UpdateFact(milestone.ID, FactPatch{Blocker: &blocker}); err != nil {
		t.Fatalf("update fact: %v", err)
	}
	expectRows(t, logAfter(t, s, mark),
		[]changeRow{{"fact", string(milestone.ID), "updated"}}, "UpdateFact")

	mark = topSeq(t, s)
	if err := s.DeleteFact(milestone.ID); err != nil {
		t.Fatalf("delete fact: %v", err)
	}
	expectRows(t, logAfter(t, s, mark),
		[]changeRow{{"fact", string(milestone.ID), "destroyed"}}, "DeleteFact")

	// TickProjects is the one write path that runs on a SCHEDULE. A tick that
	// only observes emits the project's own updated row — no owner, so nothing
	// is told — and a tick that finds nothing new emits nothing at all, which is
	// what keeps an hourly clock from moving the sync token every hour.
	mark = topSeq(t, s)
	if _, err := s.TickProjects(time.Now().UTC()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	expectRows(t, logAfter(t, s, mark),
		[]changeRow{{"project", string(project.ID), "updated"}}, "TickProjects (observed)")
	mark = topSeq(t, s)
	if _, err := s.TickProjects(time.Now().UTC()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), nil, "TickProjects (nothing new)")

	// DeleteProject CASCADES to its whole SUBTREE and to every fact under it, as
	// explicit per-row DELETEs, so a sync client is never left holding a fact —
	// or a sub-project — of a project it saw die.
	orphan, err := s.CreateFact(project.ID,
		Fact{Kind: FactNote, Title: "agent", Body: "corpsec@example.com"}, string(bot.ID))
	if err != nil {
		t.Fatalf("create fact for the cascade: %v", err)
	}
	child, err := s.CreateProject(Project{Name: "US Passport", ParentID: project.ID}, string(bot.ID))
	if err != nil {
		t.Fatalf("create sub-project: %v", err)
	}
	childFact, err := s.CreateFact(child.ID,
		Fact{Kind: FactMilestone, Title: "book the appointment"}, string(bot.ID))
	if err != nil {
		t.Fatalf("create sub-project fact: %v", err)
	}
	mark = topSeq(t, s)
	if err := s.DeleteProject(project.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"fact", string(orphan.ID), "destroyed"},
		{"fact", string(childFact.ID), "destroyed"},
		{"project", string(project.ID), "destroyed"},
		{"project", string(child.ID), "destroyed"},
	}, "DeleteProject")

	// DeleteBot tombstones everything the bot owned: both messages, both
	// segments, then the bot itself.
	mark = topSeq(t, s)
	if err := s.DeleteBot(bot.ID); err != nil {
		t.Fatalf("delete bot: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"message", msg.ID, "destroyed"},
		{"message", reply.ID, "destroyed"},
		{"segment", string(seg0.ID), "destroyed"},
		{"segment", string(seg1.ID), "destroyed"},
		{"bot", string(bot.ID), "destroyed"},
	}, "DeleteBot")
}

// TestStartupSweepEmitsChangeRows: failInterruptedSends runs inside migrate,
// not through any store helper — exactly the kind of write the triggers exist
// to catch. A reconnecting client must see the failure; and a clean reopen
// must emit nothing, or every restart would wake every client.
func TestStartupSweepEmitsChangeRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	bot := newBot(t, s)
	orphan, err := s.AppendMessage(bot.ID, "user", "answer me", StatusAwaiting)
	if err != nil {
		t.Fatalf("append awaiting: %v", err)
	}
	mark := topSeq(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if s, err = Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	expectRows(t, logAfter(t, s, mark),
		[]changeRow{{"message", orphan.ID, "updated"}}, "startup sweep")

	// A reopen with nothing to sweep is silent.
	mark = topSeq(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if s, err = Open(path); err != nil {
		t.Fatalf("clean reopen: %v", err)
	}
	defer s.Close()
	expectRows(t, logAfter(t, s, mark), nil, "clean reopen")
}

// TestLegacyMigrationEmitsChangeRows: upgrading a pre-segment database
// backfills segments, message assignments, list metadata and read watermarks —
// all writes that bypass the store helpers, all captured by the triggers.
func TestLegacyMigrationEmitsChangeRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacyDB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	changes, err := s.ChangesSince(formatState(0), 1000)
	if err != nil {
		t.Fatalf("changes since genesis: %v", err)
	}
	if got := len(changes.Changed.Segments.Created); got != 3 {
		t.Errorf("segment backfill created %d segments in the feed, want 3", got)
	}
	wantMsgs := []string{"msg_1", "msg_2", "msg_3"}
	if got := changes.Changed.Messages.Updated; !reflect.DeepEqual(got, wantMsgs) {
		t.Errorf("message backfill updated %v, want %v", got, wantMsgs)
	}
	wantBots := []string{"bot_OK", "bot_QUIET", "bot_STALE"}
	if got := changes.Changed.Bots.Updated; !reflect.DeepEqual(got, wantBots) {
		t.Errorf("bot backfill updated %v, want %v", got, wantBots)
	}
}

// TestChangesCoalescing pins the documented rules: created+updated → created,
// created+destroyed → invisible, updated+destroyed → destroyed, and a page
// consumed to the end reports the server's current state.
func TestChangesCoalescing(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	net, err := s.CreateNet("home")
	if err != nil {
		t.Fatalf("create net: %v", err)
	}

	// created then updated → created, once.
	state0, err := s.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	bot, err := s.CreateBot(net.ID, "Ada", "p", modelselector.DeepSeekV4.ID)
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	name := "renamed"
	if _, err := s.UpdateBot(bot.ID, BotPatch{DisplayName: &name}, ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	changes, err := s.ChangesSince(state0, 1000)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if got := changes.Changed.Bots; !reflect.DeepEqual(got.Created, []string{string(bot.ID)}) || len(got.Updated) != 0 {
		t.Errorf("created+updated coalesced to created=%v updated=%v, want the bot once, in created", got.Created, got.Updated)
	}
	if changes.OldState != state0 {
		t.Errorf("oldState = %q, want the requested %q", changes.OldState, state0)
	}
	if now, _ := s.State(); changes.NewState != now {
		t.Errorf("newState = %q, want the server's current %q", changes.NewState, now)
	}
	if changes.HasMoreChanges {
		t.Error("hasMoreChanges = true on a full page")
	}

	// updated only → updated.
	state1, _ := s.State()
	if _, err := s.MarkRead(bot.ID); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	changes, err = s.ChangesSince(state1, 1000)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if got := changes.Changed.Bots; !reflect.DeepEqual(got.Updated, []string{string(bot.ID)}) || len(got.Created) != 0 {
		t.Errorf("update-only window = created=%v updated=%v, want the bot once, in updated", got.Created, got.Updated)
	}

	// created then destroyed inside the window → invisible, segment included.
	state2, _ := s.State()
	ghost, err := s.CreateBot(net.ID, "Ghost", "p", modelselector.DeepSeekV4.ID)
	if err != nil {
		t.Fatalf("create ghost: %v", err)
	}
	if err := s.DeleteBot(ghost.ID); err != nil {
		t.Fatalf("delete ghost: %v", err)
	}
	changes, err = s.ChangesSince(state2, 1000)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if got := changes.Changed; len(got.Bots.Created)+len(got.Bots.Updated)+len(got.Bots.Destroyed)+
		len(got.Segments.Created)+len(got.Segments.Updated)+len(got.Segments.Destroyed) != 0 {
		t.Errorf("a bot created and destroyed in one window leaked into the feed: %+v", got)
	}
	// But the state still moved: nothing to report is not the same as nothing
	// happened.
	if now, _ := s.State(); changes.NewState != now || changes.NewState == state2 {
		t.Errorf("newState = %q (was %q), want it advanced to current %q", changes.NewState, state2, now)
	}

	// updated (long ago created) then destroyed → destroyed tombstone.
	state3, _ := s.State()
	if _, err := s.MarkRead(bot.ID); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if err := s.DeleteBot(bot.ID); err != nil {
		t.Fatalf("delete bot: %v", err)
	}
	changes, err = s.ChangesSince(state3, 1000)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if got := changes.Changed.Bots; !reflect.DeepEqual(got.Destroyed, []string{string(bot.ID)}) ||
		len(got.Created)+len(got.Updated) != 0 {
		t.Errorf("update-then-destroy = %+v, want only a destroyed tombstone", got)
	}

	// A caught-up client polls and gets nothing, with its token unchanged.
	now, _ := s.State()
	changes, err = s.ChangesSince(now, 1000)
	if err != nil {
		t.Fatalf("changes at head: %v", err)
	}
	if changes.NewState != now || changes.HasMoreChanges {
		t.Errorf("poll at head = newState %q hasMore %v, want %q and false", changes.NewState, changes.HasMoreChanges, now)
	}
}

// TestChangesPagination: a window bigger than the page limit is cut short with
// hasMoreChanges, and following NewState pages walks the whole window without
// loss or overlap.
func TestChangesPagination(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	net, err := s.CreateNet("home")
	if err != nil {
		t.Fatalf("create net: %v", err)
	}
	state0, _ := s.State()
	want := map[string]bool{}
	for i := 0; i < 5; i++ { // 5 bots → 10 raw rows (bot + segment 0 each)
		b, err := s.CreateBot(net.ID, "b", "p", modelselector.DeepSeekV4.ID)
		if err != nil {
			t.Fatalf("create bot %d: %v", i, err)
		}
		want[string(b.ID)] = true
	}

	got := map[string]bool{}
	token, pages := state0, 0
	for {
		changes, err := s.ChangesSince(token, 3)
		if err != nil {
			t.Fatalf("page from %q: %v", token, err)
		}
		for _, id := range changes.Changed.Bots.Created {
			if got[id] {
				t.Errorf("bot %s delivered twice across pages", id)
			}
			got[id] = true
		}
		if pages++; pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		if !changes.HasMoreChanges {
			break
		}
		if changes.NewState == token {
			t.Fatalf("hasMoreChanges with an unmoved token %q", token)
		}
		token = changes.NewState
	}
	if pages < 2 {
		t.Fatalf("10 rows at limit 3 took %d page(s), want several", pages)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paged bots = %v, want %v", keys(got), keys(want))
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestChangesResyncError: every state the server cannot compute from — never
// issued, ahead of the log, or behind its earliest surviving row — is the one
// distinct resync error, because the client's only correct move is the same:
// drop the cache and refetch.
func TestChangesResyncError(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s) // bot + segment: rows 1-2
	if _, err := s.AppendMessage(bot.ID, "user", "hi", StatusSent); err != nil {
		t.Fatalf("append: %v", err) // message + bot metadata: rows 3-4
	}

	for _, bad := range []string{"", "42", "banana", "s", "s-1", "sZZ!"} {
		if _, err := s.ChangesSince(bad, 1000); !errors.Is(err, ErrCannotCalculateChanges) {
			t.Errorf("ChangesSince(%q) = %v, want ErrCannotCalculateChanges", bad, err)
		}
	}

	ahead := formatState(topSeq(t, s) + 5)
	if _, err := s.ChangesSince(ahead, 1000); !errors.Is(err, ErrCannotCalculateChanges) {
		t.Errorf("a state ahead of the server = %v, want ErrCannotCalculateChanges", err)
	}

	// Prune the log's head (what a future retention policy will do): a cursor
	// from before the cut must get the resync error, not silently miss rows —
	// this is what makes pruning safe to add.
	if _, err := s.db.Exec(`DELETE FROM change_log WHERE seq <= 2`); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := s.ChangesSince(formatState(0), 1000); !errors.Is(err, ErrCannotCalculateChanges) {
		t.Errorf("pre-prune cursor = %v, want ErrCannotCalculateChanges", err)
	}
	if _, err := s.ChangesSince(formatState(1), 1000); !errors.Is(err, ErrCannotCalculateChanges) {
		t.Errorf("cursor just inside the pruned range = %v, want ErrCannotCalculateChanges", err)
	}
	// The oldest still-computable cursor works: everything after it survives.
	if _, err := s.ChangesSince(formatState(2), 1000); err != nil {
		t.Errorf("oldest surviving cursor errored: %v", err)
	}
}

// ── HTTP surface ──────────────────────────────────────────────────────────────

// getWithState GETs and returns the decoded body plus the X-BotNet-State
// header the collection endpoints now carry.
func getWithState(t *testing.T, url string, out any) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d (%s)", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp.Header.Get("X-BotNet-State")
}

// TestStateHeaderOnCollectionGets: every shipped collection endpoint carries
// the sync token in X-BotNet-State — bodies stay byte-identical — and the
// token moves when the data does.
func TestStateHeaderOnCollectionGets(t *testing.T) {
	h := newHarness(t, &fakeLLM{reply: "hi"})

	before := getWithState(t, h.ts.URL+"/v1/bots", nil)
	if before == "" {
		t.Fatal("GET /v1/bots carries no X-BotNet-State header")
	}

	bot := createBot(t, h, "Ada")
	after := getWithState(t, h.ts.URL+"/v1/bots", nil)
	if after == before {
		t.Errorf("state token %q did not move across a mutation", after)
	}

	if got := getWithState(t, h.bot(bot.ID, "/messages"), nil); got == "" {
		t.Error("GET messages carries no X-BotNet-State header")
	}
	if got := getWithState(t, h.bot(bot.ID, "/segments"), nil); got == "" {
		t.Error("GET segments carries no X-BotNet-State header")
	}
}

// TestChangesEndpoint drives the sync loop a client will run: bootstrap a
// token from a collection GET, mutate, poll /v1/changes, fetch the named ids,
// and poll again to find itself caught up.
func TestChangesEndpoint(t *testing.T) {
	h := newHarness(t, &fakeLLM{reply: "hello from the bot"})

	token := getWithState(t, h.ts.URL+"/v1/bots", nil)
	bot := createBot(t, h, "Ada")
	conv := sendAndSettle(t, h, bot.ID, `"hi"`)
	if len(conv) != 2 {
		t.Fatalf("transcript has %d messages, want 2", len(conv))
	}

	var changes Changes
	getWithState(t, h.ts.URL+"/v1/changes?since="+token, &changes)
	if !reflect.DeepEqual(changes.Changed.Bots.Created, []string{string(bot.ID)}) {
		t.Errorf("changes.bots.created = %v, want [%s]", changes.Changed.Bots.Created, bot.ID)
	}
	wantMsgs := []string{conv[0].ID, conv[1].ID}
	sort.Strings(wantMsgs)
	if !reflect.DeepEqual(changes.Changed.Messages.Created, wantMsgs) {
		t.Errorf("changes.messages.created = %v, want %v", changes.Changed.Messages.Created, wantMsgs)
	}
	if len(changes.Changed.Segments.Created) != 1 {
		t.Errorf("changes.segments.created = %v, want the bot's segment 0", changes.Changed.Segments.Created)
	}

	// Fetch the messages the feed named, in one batch.
	var msgs []Message
	getWithState(t, h.ts.URL+"/v1/messages?ids="+strings.Join(wantMsgs, ","), &msgs)
	if len(msgs) != 2 {
		t.Fatalf("batch fetch returned %d messages, want 2", len(msgs))
	}
	// Insertion order, regardless of the order ids were asked for.
	if msgs[0].ID != conv[0].ID || msgs[1].ID != conv[1].ID {
		t.Errorf("batch fetch order = [%s %s], want transcript order [%s %s]",
			msgs[0].ID, msgs[1].ID, conv[0].ID, conv[1].ID)
	}

	// Caught up: polling from newState returns empty buckets, same token.
	var again Changes
	getWithState(t, h.ts.URL+"/v1/changes?since="+changes.NewState, &again)
	if len(again.Changed.Bots.Created)+len(again.Changed.Messages.Created) != 0 || again.NewState != changes.NewState {
		t.Errorf("caught-up poll = %+v, want empty with token %q", again, changes.NewState)
	}
}

// TestChangesEndpointErrors: no since is a 400; a since the server cannot
// compute from is 410 with the distinct cannotCalculateChanges code, which is
// what tells a client to resync rather than retry.
func TestChangesEndpointErrors(t *testing.T) {
	h := newHarness(t, &fakeLLM{})

	if code, _ := getRaw(t, h.ts.URL+"/v1/changes"); code != http.StatusBadRequest {
		t.Errorf("GET /v1/changes without since = %d, want 400", code)
	}

	code, body := getRaw(t, h.ts.URL+"/v1/changes?since=banana")
	if code != http.StatusGone {
		t.Errorf("GET /v1/changes with a bogus since = %d, want 410", code)
	}
	var payload struct{ Code string }
	if err := json.Unmarshal([]byte(body), &payload); err != nil || payload.Code != "cannotCalculateChanges" {
		t.Errorf("resync body = %q, want code cannotCalculateChanges", body)
	}
}

// TestLongPollAnswersWhenSomethingChanges: a caught-up ?wait= poll parks, then
// answers promptly once a mutation lands — well inside the wait, carrying the
// change, in exactly the plain poll's shape.
func TestLongPollAnswersWhenSomethingChanges(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	token := getWithState(t, h.ts.URL+"/v1/bots", nil)

	type result struct {
		changes Changes
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		var c Changes
		getWithState(t, h.ts.URL+"/v1/changes?since="+token+"&wait=10s", &c)
		done <- result{c, time.Since(start)}
	}()

	// Let the poll park, then mutate.
	time.Sleep(150 * time.Millisecond)
	bot := createBot(t, h, "Ada")

	select {
	case res := <-done:
		if res.elapsed >= 5*time.Second {
			t.Errorf("long-poll took %v, want an answer well inside the 10s wait", res.elapsed)
		}
		if !reflect.DeepEqual(res.changes.Changed.Bots.Created, []string{string(bot.ID)}) {
			t.Errorf("long-poll answered %+v, want the created bot", res.changes.Changed)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("long-poll never answered a change")
	}
}

// TestLongPollTimesOutEmpty: nothing changing answers empty after the wait,
// token unmoved — the client's cue to simply poll again.
func TestLongPollTimesOutEmpty(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	token := getWithState(t, h.ts.URL+"/v1/bots", nil)

	start := time.Now()
	var c Changes
	getWithState(t, h.ts.URL+"/v1/changes?since="+token+"&wait=300ms", &c)
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Errorf("empty long-poll answered after %v, want it to hold the full 300ms", elapsed)
	}
	if c.NewState != token || len(c.Changed.Bots.Created) != 0 {
		t.Errorf("timed-out poll = %+v, want empty with token %q", c, token)
	}

	// A poll that is already behind answers immediately, wait or no wait.
	createBot(t, h, "Ada")
	var behind Changes
	getWithState(t, h.ts.URL+"/v1/changes?since="+token+"&wait=10s", &behind)
	if len(behind.Changed.Bots.Created) != 1 {
		t.Errorf("behind long-poll = %+v, want the pending change at once", behind.Changed)
	}

	if code, _ := getRaw(t, h.ts.URL+"/v1/changes?since="+token+"&wait=banana"); code != http.StatusBadRequest {
		t.Errorf("bogus wait = %d, want 400", code)
	}
}

// TestBatchFetchMessages: unknown ids are silently absent (destroyed after the
// feed named them), no ids is a 400, and the id-count cap is enforced.
func TestBatchFetchMessages(t *testing.T) {
	h := newHarness(t, &fakeLLM{reply: "yo"})
	bot := createBot(t, h, "Ada")
	conv := sendAndSettle(t, h, bot.ID, `"hi"`)

	var msgs []Message
	getWithState(t, h.ts.URL+"/v1/messages?ids="+conv[0].ID+",msg_NOSUCH", &msgs)
	if len(msgs) != 1 || msgs[0].ID != conv[0].ID {
		t.Errorf("batch fetch with an unknown id = %v, want just %s", msgs, conv[0].ID)
	}

	if code, _ := getRaw(t, h.ts.URL+"/v1/messages"); code != http.StatusBadRequest {
		t.Errorf("GET /v1/messages without ids = %d, want 400", code)
	}

	many := strings.Repeat("msg_X,", maxBatchIDs) + "msg_X"
	if code, _ := getRaw(t, h.ts.URL+"/v1/messages?ids="+many); code != http.StatusBadRequest {
		t.Errorf("GET /v1/messages beyond the id cap = %d, want 400", code)
	}
}
