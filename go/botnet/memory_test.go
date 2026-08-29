package botnet

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// Per-bot editable memory: the store round-trip, the migration of pre-memory
// databases, the PATCH surface, and the rule that memory writes never move the
// derived Version (so a model taking notes mid-chat cannot 412 a user's edit).

// TestMemoryRoundTrip: a bot starts with empty memory, SetMemory replaces the
// whole blob, and the value comes back on every read path the UI uses.
func TestMemoryRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)
	if bot.Memory != "" {
		t.Errorf("new bot memory = %q, want empty", bot.Memory)
	}

	set, err := s.SetMemory(bot.ID, "the user's name is Y")
	if err != nil {
		t.Fatalf("set memory: %v", err)
	}
	if set.Memory != "the user's name is Y" {
		t.Errorf("SetMemory returned memory %q", set.Memory)
	}
	got, err := s.GetBot(bot.ID)
	if err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if got.Memory != "the user's name is Y" {
		t.Errorf("memory after round-trip = %q", got.Memory)
	}

	// The listing carries it too — the details panel draws from ListBots.
	net, err := s.EnsureDefaultNet()
	if err != nil {
		t.Fatalf("ensure net: %v", err)
	}
	bots, err := s.ListBots(net.ID)
	if err != nil {
		t.Fatalf("list bots: %v", err)
	}
	if len(bots) != 1 || bots[0].Memory != "the user's name is Y" {
		t.Errorf("listing = %+v, want it to carry the memory", bots)
	}

	// Empty string is a real value: it clears.
	if cleared, err := s.SetMemory(bot.ID, ""); err != nil || cleared.Memory != "" {
		t.Errorf("clearing memory = (%q, %v), want empty and no error", cleared.Memory, err)
	}
	if _, err := s.SetMemory("bot_NOPE", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetMemory on a missing bot = %v, want ErrNotFound", err)
	}
}

// TestMemoryWriteDoesNotMoveVersion pins the DECISION: memory is not an
// authored field, so a memory write — the model's mid-chat note-taking — must
// not invalidate an If-Match edit the user has in flight.
func TestMemoryWriteDoesNotMoveVersion(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)

	after, err := s.SetMemory(bot.ID, "notes taken mid-chat")
	if err != nil {
		t.Fatalf("set memory: %v", err)
	}
	if after.Version != bot.Version {
		t.Fatalf("a memory write moved the version %q → %q; chat would spuriously 412 edits",
			bot.Version, after.Version)
	}

	// The user's conditional prompt edit, carrying the version read BEFORE the
	// model wrote memory, still lands — and keeps the memory.
	prompt := "be terse"
	edited, err := s.UpdateBot(bot.ID, BotPatch{SystemPrompt: &prompt}, bot.Version)
	if err != nil {
		t.Fatalf("conditional edit after a memory write: %v", err)
	}
	if edited.Memory != "notes taken mid-chat" {
		t.Errorf("the prompt edit clobbered memory: %q", edited.Memory)
	}
	// A prompt edit DOES move the version, so a genuinely stale edit still 412s.
	if edited.Version == bot.Version {
		t.Fatal("a prompt edit did not move the version")
	}
	stale := "be verbose"
	if _, err := s.UpdateBot(bot.ID, BotPatch{SystemPrompt: &stale}, bot.Version); !errors.Is(err, ErrVersionMismatch) {
		t.Errorf("stale conditional edit = %v, want ErrVersionMismatch", err)
	}
}

// TestMemoryMigration: a database from before the memory column exists gets it
// on Open, with every existing bot at "" — and a written memory survives the
// next migration pass rather than being re-defaulted.
func TestMemoryMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacyDB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	bots, err := s.ListBots("net_LEGACY")
	if err != nil {
		t.Fatalf("list bots: %v", err)
	}
	if len(bots) != 3 {
		t.Fatalf("listed %d bots, want the 3 seeded", len(bots))
	}
	for _, b := range bots {
		if b.Memory != "" {
			t.Errorf("migrated bot %s memory = %q, want empty", b.ID, b.Memory)
		}
	}

	if _, err := s.SetMemory("bot_OK", "kept across reopen"); err != nil {
		t.Fatalf("set memory: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if s, err = Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, err := s.GetBot("bot_OK")
	if err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if got.Memory != "kept across reopen" {
		t.Errorf("memory after reopen = %q, want it kept", got.Memory)
	}
}

// TestPatchMemory drives the user's edit path over HTTP: set, leave alone when
// omitted, clear with "", and never move the version.
func TestPatchMemory(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	bot := createBot(t, h, "Ada")

	var edited Bot
	patch(t, h.bot(bot.ID, ""), `{"memory":"likes short answers"}`, &edited)
	if edited.Memory != "likes short answers" {
		t.Fatalf("patched memory = %q", edited.Memory)
	}
	if edited.Version != bot.Version {
		t.Errorf("a memory PATCH moved the version %q → %q; memory is not an authored field",
			bot.Version, edited.Version)
	}

	// Omitting the field leaves it alone.
	patch(t, h.bot(bot.ID, ""), `{"displayName":"Ada II"}`, &edited)
	if edited.Memory != "likes short answers" {
		t.Errorf("a memory-less PATCH changed memory to %q", edited.Memory)
	}

	// Empty string is a valid value: it clears.
	patch(t, h.bot(bot.ID, ""), `{"memory":""}`, &edited)
	if edited.Memory != "" {
		t.Errorf("PATCH memory:\"\" left %q, want it cleared", edited.Memory)
	}

	// The user path stays conditional like any PATCH: a stale If-Match 412s.
	req, err := http.NewRequest(http.MethodPatch, h.bot(bot.ID, ""),
		strings.NewReader(`{"memory":"x"}`))
	if err != nil {
		t.Fatalf("build patch: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"v_stale"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("memory PATCH under a stale If-Match = %d (%s), want 412", resp.StatusCode, raw)
	}
}

// TestTurnCarriesMemoryAndTools: the server hands every model turn the bot's
// memory blob and a toolbox bound to that same bot — which is what lines the
// injected block and the tools up with the bot being spoken to.
func TestTurnCarriesMemoryAndTools(t *testing.T) {
	llm := &fakeLLM{reply: "ok"}
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")
	if _, err := h.store.SetMemory(bot.ID, "remember: prefers Go"); err != nil {
		t.Fatalf("set memory: %v", err)
	}

	sendAndSettle(t, h, bot.ID, `"hi"`)
	p := llm.lastPrompt(t)
	if p.Memory != "remember: prefers Go" {
		t.Errorf("turn memory = %q, want the bot's blob", p.Memory)
	}
	if p.Tools == nil {
		t.Fatal("turn carries no toolbox")
	}
	// The toolbox is bound to THIS bot: a memory read returns its blob.
	if got, err := p.Tools.Run(context.Background(), "memory", []byte(`{"command":"read"}`)); err != nil || got.text != "remember: prefers Go" {
		t.Errorf("toolbox memory read = (%q, %v), want the bot's memory", got.text, err)
	}

	// A bot with no memory hands the turn an empty blob (nothing is injected).
	other := createBot(t, h, "Ben")
	sendAndSettle(t, h, other.ID, `"hi"`)
	if p := llm.lastPrompt(t); p.Memory != "" {
		t.Errorf("a bot with empty memory was handed %q", p.Memory)
	}
}
