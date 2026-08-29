package botnet

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Write-path hardening: idempotent sends under client-supplied ids, and
// conditional PATCH under If-Match.

// TestIdempotentSendReplay: a send whose response was lost is retried with the
// same client-minted id and must be a no-op — the stored turn comes back, no
// second model call starts, and the feed's state does not move.
func TestIdempotentSendReplay(t *testing.T) {
	llm := &fakeLLM{reply: "the reply"}
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")

	id := newID("msg_")
	body := `{"id":"` + id + `","content":"hello"}`

	var first Message
	postExpect(t, http.StatusAccepted, h.bot(bot.ID, "/messages"), body, &first)
	if first.ID != id {
		t.Fatalf("send stored id %q, want the client-supplied %q", first.ID, id)
	}
	h.settle()

	stateBefore, err := h.store.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	calls := llm.promptCount()

	// The retry: same id, same body. 200 (not 202) with the settled original.
	var replay Message
	postExpect(t, http.StatusOK, h.bot(bot.ID, "/messages"), body, &replay)
	if replay.ID != id || replay.Status != StatusSent {
		t.Errorf("replay = %+v, want the original message, already settled", replay)
	}
	if got := llm.promptCount(); got != calls {
		t.Errorf("replay started a model turn (%d calls, was %d) — the turn is duplicated", got, calls)
	}
	if stateAfter, _ := h.store.State(); stateAfter != stateBefore {
		t.Errorf("replay moved the sync state %q → %q; a no-op must emit no change rows", stateBefore, stateAfter)
	}

	// The transcript holds exactly one user turn and one reply — no duplicate.
	var conv []Message
	get(t, h.bot(bot.ID, "/messages"), &conv)
	if len(conv) != 2 {
		t.Errorf("transcript has %d messages after a replay, want 2", len(conv))
	}
}

// TestReplayWhileOriginalInFlight: retrying a send whose original is still
// awaiting returns that awaiting row rather than tripping over the bot being
// busy — with its own turn.
func TestReplayWhileOriginalInFlight(t *testing.T) {
	llm := &fakeLLM{reply: "eventually"}
	llm.hold()
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")

	id := newID("msg_")
	body := `{"id":"` + id + `","content":"hello"}`
	var first Message
	postExpect(t, http.StatusAccepted, h.bot(bot.ID, "/messages"), body, &first)
	llm.waitForCall(t, 1)

	var replay Message
	postExpect(t, http.StatusOK, h.bot(bot.ID, "/messages"), body, &replay)
	if replay.ID != id || replay.Status != StatusAwaiting {
		t.Errorf("mid-flight replay = %+v, want the original, still awaiting", replay)
	}
}

// TestClientMessageIDValidation: the id is client input reaching a primary
// key, so anything but the exact minted shape is a 400.
func TestClientMessageIDValidation(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	bot := createBot(t, h, "Ada")

	for _, bad := range []string{
		"msg_short",                          // wrong length
		"bot_01ARZ3NDEKTSV4RRFFQ69G5FAV",     // wrong prefix
		"msg_01arz3ndektsv4rrffq69g5fav",     // lowercase
		"msg_01ARZ3NDEKTSV4RRFFQ69G5FAI",     // 'I' is not Crockford
		"msg_01ARZ3NDEKTSV4RRFFQ69G5FAVX",    // too long
		"msg_01ARZ3NDEKTSV4RRFFQ69G5FA ",     // trailing space
		"msg_01ARZ3NDEKTSV4RRFFQ69G5F!V",     // punctuation
	} {
		code, _ := postRaw(t, h.bot(bot.ID, "/messages"), `{"id":"`+bad+`","content":"x"}`)
		if code != http.StatusBadRequest {
			t.Errorf("send with id %q = %d, want 400", bad, code)
		}
	}
}

// TestClientMessageIDWrongBot: one id names one message forever; replaying it
// against a different bot is a conflict, never a silent adoption.
func TestClientMessageIDWrongBot(t *testing.T) {
	h := newHarness(t, &fakeLLM{reply: "ok"})
	ada := createBot(t, h, "Ada")
	ben := createBot(t, h, "Ben")

	id := newID("msg_")
	var sent Message
	postExpect(t, http.StatusAccepted, h.bot(ada.ID, "/messages"), `{"id":"`+id+`","content":"hi"}`, &sent)
	h.settle()

	code, body := postRaw(t, h.bot(ben.ID, "/messages"), `{"id":"`+id+`","content":"hi"}`)
	if code != http.StatusConflict {
		t.Errorf("same id on another bot = %d (%s), want 409", code, body)
	}

	// Store-level: the sentinel is distinct from ErrBusy.
	if _, _, err := h.store.AppendMessageAs(id, ben.ID, "user", "hi", StatusAwaiting); !errors.Is(err, ErrIDConflict) {
		t.Errorf("AppendMessageAs on the wrong bot = %v, want ErrIDConflict", err)
	}
}

// TestIfMatchOnPatch drives the conditional-edit story: a PATCH carrying the
// version it read succeeds, a PATCH carrying a version that has since moved
// fails with 412, and a replayed PATCH — which by definition carries the
// pre-edit version — fails the same way, which is the correct replay answer.
func TestIfMatchOnPatch(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	bot := createBot(t, h, "Ada")
	if bot.Version == "" {
		t.Fatal("created bot carries no version")
	}

	patchIf := func(version, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPatch, h.bot(bot.ID, ""), strings.NewReader(body))
		if err != nil {
			t.Fatalf("build patch: %v", err)
		}
		if version != "" {
			req.Header.Set("If-Match", `"`+version+`"`)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("patch: %v", err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read patch response: %v", err)
		}
		return resp.StatusCode, string(raw)
	}

	// Conditioned on the version the client read: succeeds, version moves.
	code, body := patchIf(bot.Version, `{"systemPrompt":"be terse"}`)
	if code != http.StatusOK {
		t.Fatalf("conditional patch = %d (%s), want 200", code, body)
	}
	var edited Bot
	if err := json.Unmarshal([]byte(body), &edited); err != nil {
		t.Fatalf("decode edited bot: %v", err)
	}
	if edited.Version == bot.Version {
		t.Fatal("an edit did not move the bot's version")
	}

	// The replayed (or concurrent) PATCH carries the now-stale version: 412.
	if code, body := patchIf(bot.Version, `{"systemPrompt":"be verbose"}`); code != http.StatusPreconditionFailed {
		t.Errorf("stale-version patch = %d (%s), want 412", code, body)
	}
	// And the losing edit changed nothing.
	after, err := h.store.GetBot(bot.ID)
	if err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if after.SystemPrompt != "be terse" {
		t.Errorf("system prompt = %q after a refused patch, want the first edit kept", after.SystemPrompt)
	}

	// No If-Match stays unconditional, exactly as shipped clients behave.
	if code, body := patchIf("", `{"displayName":"Ada II"}`); code != http.StatusOK {
		t.Errorf("unconditional patch = %d (%s), want 200", code, body)
	}

	// Message traffic must NOT move the version: it is authored fields only.
	before, _ := h.store.GetBot(bot.ID)
	if _, err := h.store.AppendMessage(bot.ID, "user", "chatter", StatusSent); err != nil {
		t.Fatalf("append: %v", err)
	}
	afterMsg, _ := h.store.GetBot(bot.ID)
	if afterMsg.Version != before.Version {
		t.Error("a message append moved the bot's edit version; chat would spuriously 412 edits")
	}
}
