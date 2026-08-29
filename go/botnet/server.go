package botnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	modelselector "stdtools/go/lib/modelSelector"
)

// turnTimeout bounds one background model call. It is a backstop, not a policy:
// a wedged call would otherwise hold the bot's single in-flight slot forever and
// no further sends would be accepted.
const turnTimeout = 5 * time.Minute

// Server is the botnet state owner: it holds the single Store and the LLM, and
// exposes the HTTP API the UI calls. Every state change (create bot, send
// message, compact) goes through here — clients keep no state of their own.
//
// Sending is asynchronous: the handler persists the user's turn and returns,
// and the model call runs on a goroutine tracked by turns. Nothing about the
// outcome is reported to that original caller — it is written to the store,
// which is the only place the client reads from anyway.
type Server struct {
	store   *Store
	llm     LLM
	netID   string
	keyPath string  // where SetKey persists the OpenRouter key; "" disables persistence
	search  *Router // web-search backend router; nil disables the client web_search tool
	turns   sync.WaitGroup
}

// Wait blocks until every in-flight model call has finished. Tests use it to
// close the store without racing a background turn; botnetd serves until the
// process exits and never calls it.
func (s *Server) Wait() { s.turns.Wait() }

// ConfigureKeyPersistence tells the server where to save a key set via the
// config endpoint, so it survives a restart.
func (s *Server) ConfigureKeyPersistence(path string) { s.keyPath = path }

// ConfigureSearch installs the web-search backend router. botnetd builds it from
// the environment (NewRouterFromEnv) and calls this; a server left unconfigured
// (as in tests) has a nil router, offers no client web_search tool, and keeps
// falling back to OpenRouter's server tool. Kept out of NewServer so ambient
// provider keys cannot make the test suite non-deterministic.
func (s *Server) ConfigureSearch(r *Router) { s.search = r }

// NewServer wires a store and an LLM into an HTTP handler, ensuring a default
// net exists to own the bots.
func NewServer(store *Store, llm LLM) (*Server, error) {
	net, err := store.EnsureDefaultNet()
	if err != nil {
		return nil, err
	}
	return &Server{store: store, llm: llm, netID: net.ID}, nil
}

// Handler returns the routed HTTP mux for the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/config", s.getConfig)
	mux.HandleFunc("POST /v1/config", s.setConfig)
	mux.HandleFunc("GET /v1/models", s.listModels)
	mux.HandleFunc("GET /v1/tools", s.listTools)
	mux.HandleFunc("GET /v1/bots", s.listBots)
	mux.HandleFunc("POST /v1/bots", s.createBot)
	mux.HandleFunc("PATCH /v1/bots/{id}", s.patchBot)
	mux.HandleFunc("DELETE /v1/bots/{id}", s.deleteBot)
	mux.HandleFunc("POST /v1/bots/{id}/read", s.markRead)
	mux.HandleFunc("GET /v1/bots/{id}/messages", s.getMessages)
	mux.HandleFunc("POST /v1/bots/{id}/messages", s.sendMessage)
	mux.HandleFunc("POST /v1/bots/{id}/messages/{messageId}/retry", s.retryMessage)
	mux.HandleFunc("GET /v1/bots/{id}/segments", s.getSegments)
	mux.HandleFunc("POST /v1/bots/{id}/compact", s.compact)
	mux.HandleFunc("GET /v1/messages/{id}", s.getMessage)
	mux.HandleFunc("GET /v1/messages", s.batchMessages)
	mux.HandleFunc("GET /v1/changes", s.getChanges)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// changePageLimit caps one /v1/changes page at this many raw log rows.
const changePageLimit = 1000

// maxChangeWait caps ?wait= on /v1/changes; longer requests are clamped, not
// refused — the client just re-polls sooner than it asked to.
const maxChangeWait = 60 * time.Second

// changeWaitTick is how often a waiting /v1/changes re-checks the log.
// DECISION: the wake is a re-check on a short tick, not an in-process
// broadcast. A broadcast would need a bump at every mutating call site — the
// exact remember-everywhere convention the triggers were chosen to eliminate —
// while the tick catches every write the triggers catch, by construction. The
// cost is one indexed MAX(seq) per tick and ≤ one tick of extra latency; a
// broadcast can replace it later without touching the API.
const changeWaitTick = 100 * time.Millisecond

// stateHeader stamps the current sync token on a collection response. It is
// read BEFORE the collection so the token can only ever be older than the data
// it rides — a client that then polls /v1/changes refetches at worst, rather
// than trusting a token for data it was never sent.
func (s *Server) stateHeader(w http.ResponseWriter) {
	if state, err := s.store.State(); err == nil {
		w.Header().Set("X-BotNet-State", state)
	}
}

// getChanges is the sync feed: everything that moved since the client's state
// token, as ids bucketed by entity type — never hydrated objects; the client
// fetches what it wants and skips what it has. A client with no state
// bootstraps with the collection GETs, whose X-BotNet-State header is where
// tokens come from in the first place.
//
// A token the server cannot compute from — unknown, or older than the log
// reaches — is 410 with code "cannotCalculateChanges": drop the cache, refetch
// everything, resume from the new header token.
// With ?wait= (e.g. ?wait=30s) the poll becomes a long-poll: a caught-up
// request parks until something changes or the wait elapses, then answers with
// the exact same shape as the plain poll — same endpoint, same cursor, same
// payload, so the client's sync loop does not change, only its latency. An
// empty answer after the full wait is normal; the client just polls again.
func (s *Server) getChanges(w http.ResponseWriter, r *http.Request) {
	since := r.URL.Query().Get("since")
	if since == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"since is required; bootstrap from a collection GET's X-BotNet-State header"))
		return
	}
	var wait time.Duration
	if raw := r.URL.Query().Get("wait"); raw != "" {
		var err error
		if wait, err = time.ParseDuration(raw); err != nil || wait < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("wait %q is not a duration like 30s", raw))
			return
		}
		wait = min(wait, maxChangeWait)
	}

	deadline := time.Now().Add(wait)
	for {
		changes, err := s.store.ChangesSince(since, changePageLimit)
		if errors.Is(err, ErrCannotCalculateChanges) {
			writeJSON(w, http.StatusGone, map[string]string{
				"error": err.Error(),
				"code":  "cannotCalculateChanges",
			})
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		// NewState moving is the "something happened" test — it also covers a
		// window whose only events coalesced away (created+destroyed), which
		// must still answer so the client's token advances.
		if changes.NewState != changes.OldState || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, changes)
			return
		}
		select {
		case <-r.Context().Done():
			return // client hung up; nothing to answer
		case <-time.After(changeWaitTick):
		}
	}
}

// batchMessages fetches messages by id — the second half of the feed's
// ids-only contract. An id with no row is silently absent: to a feed consumer
// it was destroyed after the page named it. At most 500 ids per call.
func (s *Server) batchMessages(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("ids")
	if raw == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("ids is required, comma-separated"))
		return
	}
	s.stateHeader(w)
	msgs, err := s.store.MessagesByIDs(strings.Split(raw, ","))
	if err != nil {
		writeErr(w, writeStatus(err), err)
		return
	}
	if msgs == nil {
		msgs = []Message{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

// keyConfigurable is implemented by the OpenRouter LLM; the fake in tests is
// not, so config calls degrade gracefully there.
type keyConfigurable interface {
	SetKey(string)
	HasKey() bool
}

// getConfig reports whether the server has an OpenRouter key (never the key
// itself).
func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	hasKey := false
	if kc, ok := s.llm.(keyConfigurable); ok {
		hasKey = kc.HasKey()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"hasKey": hasKey})
}

// setConfig sets the OpenRouter key at runtime and persists it (0600) so it
// survives a restart. The key lives only on the server.
func (s *Server) setConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OpenRouterKey string `json:"openRouterKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	kc, ok := s.llm.(keyConfigurable)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("this server's LLM has no configurable key"))
		return
	}
	kc.SetKey(in.OpenRouterKey)
	if s.keyPath != "" {
		if err := os.WriteFile(s.keyPath, []byte(in.OpenRouterKey), 0o600); err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("persist key: %w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"hasKey": in.OpenRouterKey != ""})
}

// listModels surfaces the modelSelector roster so the UI never hardcodes it.
// It is also the repair menu for a bot whose stored model no longer resolves.
func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, modelselector.All())
}

// listTools surfaces the exact tool definitions the model is sent — the same
// toolWireDefs() array the chat request marshals as its "tools" key — so the
// UI shows what the model is actually told and can never drift from it. The
// list is derived from the binary's memoryCommands registry, not stored data:
// it changes only on deploy, so it is unversioned and outside the change feed.
func (s *Server) listTools(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, toolWireDefs(s.search))
}

// listBots returns every bot with its list metadata, most recently active
// first, so the sidebar draws itself from this one call. A bot whose stored
// model has left the roster is still listed, with modelValid false.
func (s *Server) listBots(w http.ResponseWriter, _ *http.Request) {
	s.stateHeader(w)
	bots, err := s.store.ListBots(s.netID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if bots == nil {
		bots = []Bot{}
	}
	writeJSON(w, http.StatusOK, bots)
}

func (s *Server) createBot(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DisplayName  string `json:"displayName"`
		SystemPrompt string `json:"systemPrompt"`
		Model        string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.DisplayName == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("displayName is required"))
		return
	}
	bot, err := s.store.CreateBot(s.netID, in.DisplayName, in.SystemPrompt, modelselector.ModelID(in.Model))
	if err != nil {
		writeErr(w, writeStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, bot)
}

// patchBot changes any of displayName, systemPrompt and model on an existing
// bot. Omitted fields are left alone. This is the repair path for a bot whose
// stored model no longer resolves — it is edited, not deleted and recreated.
//
// An If-Match header carrying the bot's version (the derived Version field)
// makes the edit conditional: 412 if the bot was edited since the client read
// it, instead of silently overwriting the other edit. No header keeps the
// unconditional behavior every shipped client already has.
func (s *Server) patchBot(w http.ResponseWriter, r *http.Request) {
	var p BotPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ifVersion := strings.Trim(r.Header.Get("If-Match"), `"`)
	bot, err := s.store.UpdateBot(BotID(r.PathValue("id")), p, ifVersion)
	if err != nil {
		writeErr(w, writeStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, bot)
}

func (s *Server) deleteBot(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteBot(BotID(r.PathValue("id"))); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// markRead stamps the bot read as of now; it is unread again as soon as a
// message arrives after that stamp.
func (s *Server) markRead(w http.ResponseWriter, r *http.Request) {
	bot, err := s.store.MarkRead(BotID(r.PathValue("id")))
	if err != nil {
		writeErr(w, writeStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, bot)
}

// getMessages returns the FULL transcript across every segment, in order.
// Compaction changes what the model is sent, never what this returns.
//
// With ?after={messageID} it returns only what follows that message, which is
// how a client polls for a reply without refetching the transcript it already
// has. The cursor is a message id — the same thing an event stream would carry —
// so replacing this poll with server-sent events later changes neither the
// cursor nor the Message shape. An unknown cursor is a 404 rather than an empty
// list, so a client holding a stale id learns it must resync instead of waiting
// forever on messages that will never come.
func (s *Server) getMessages(w http.ResponseWriter, r *http.Request) {
	s.stateHeader(w)
	botID := BotID(r.PathValue("id"))
	var msgs []Message
	var err error
	if after := r.URL.Query().Get("after"); after != "" {
		msgs, err = s.store.MessagesAfter(botID, after)
	} else {
		msgs, err = s.store.Conversation(botID)
	}
	if err != nil {
		writeErr(w, writeStatus(err), err)
		return
	}
	if msgs == nil {
		msgs = []Message{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

// getMessage looks one message up by id, with no bot in the path. A client that
// holds the id returned by a send uses this to watch that turn settle.
func (s *Server) getMessage(w http.ResponseWriter, r *http.Request) {
	msg, err := s.store.GetMessage(r.PathValue("id"))
	if err != nil {
		writeErr(w, writeStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// getSegments returns the bot's segment chain for the details panel: each
// segment with its index, open and seal times, and cumulative summary.
func (s *Server) getSegments(w http.ResponseWriter, r *http.Request) {
	s.stateHeader(w)
	segs, err := s.store.Segments(BotID(r.PathValue("id")))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if segs == nil {
		segs = []Segment{}
	}
	writeJSON(w, http.StatusOK, segs)
}

// compact is the user's Compact button: seal the open segment with a summary
// folding the previous summary and this segment's messages into one, then open
// a fresh empty segment. Messages are never deleted. Compacting an empty open
// segment is a no-op — the chain comes back unchanged rather than gaining an
// empty sealed segment.
func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	botID := BotID(r.PathValue("id"))
	bot, err := s.store.GetBot(botID)
	if err != nil {
		writeErr(w, writeStatus(err), err)
		return
	}
	seg, err := s.store.OpenSegment(botID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	msgs, err := s.store.SegmentMessages(seg.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(msgs) == 0 {
		s.writeChain(w, botID, http.StatusOK)
		return
	}
	if !bot.ModelValid {
		writeErr(w, http.StatusConflict, unusableModel(bot))
		return
	}
	// Sealing mid-reply would strand the question in the sealed segment and land
	// its answer in the new one, and summarize a segment whose last turn has not
	// happened yet. Refuse, the same way a concurrent send is refused.
	if _, err := s.store.InFlight(botID); err == nil {
		s.writeSendError(w, botID, ErrBusy)
		return
	} else if !errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// The ONE summary carried forward — not a list. Folding it in here is what
	// keeps the prompt constant-size however many times this bot is compacted.
	previous, err := s.store.LatestSummary(botID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	summary, err := s.llm.Summarize(r.Context(), bot, previous, msgs)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if err := s.store.Seal(seg, summary); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.writeChain(w, botID, http.StatusOK)
}

func (s *Server) writeChain(w http.ResponseWriter, botID BotID, code int) {
	segs, err := s.store.Segments(botID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if segs == nil {
		segs = []Segment{}
	}
	writeJSON(w, code, segs)
}

// sendMessage persists the user's turn and returns 202 immediately, WITHOUT
// waiting on the model. That wait is the whole reason this is async: a live
// model call takes seconds, and while the request blocked, the user's own
// message existed nowhere the UI could render, so sending looked like it had
// swallowed the text.
//
// The body is that one Message, with its id and Status "awaiting". The reply
// lands later, in the background; the client watches for it with
// GET /v1/bots/{id}/messages?after={that id}.
//
// A send arriving while a reply is already in flight is REFUSED with 409, not
// queued — see the ordering DECISION on Message for why that is what keeps the
// transcript in order.
// An optional client-supplied "id" ("msg_" ULID) makes the send idempotent: a
// retry after a lost response finds its original and gets it back as a 200
// with no second turn started, so the turn can never be duplicated. The same
// id on a different bot is a 409. Without an id the server mints one, exactly
// as before.
func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	botID := BotID(r.PathValue("id"))
	var in struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.ID != "" && !validMessageID(in.ID) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"id %q is not a msg_-prefixed ULID; omit it to have the server mint one", in.ID))
		return
	}
	bot, err := s.store.GetBot(botID)
	if err != nil {
		writeErr(w, writeStatus(err), err)
		return
	}
	msg, replayed, err := s.store.AppendMessageAs(in.ID, botID, "user", in.Content, StatusAwaiting)
	if err != nil {
		s.writeSendError(w, botID, err)
		return
	}
	if replayed {
		// The original send already ran (or is running) this turn.
		writeJSON(w, http.StatusOK, msg)
		return
	}
	s.startTurn(bot, msg)
	writeJSON(w, http.StatusAccepted, msg)
}

// retryMessage re-runs the model for a user turn that was left stranded — by a
// failed call, or by a process that died mid-reply — so the user never retypes
// it. Like a send it returns 202 and the model call runs in the background. The
// message must still be in the open segment and not already answered.
func (s *Server) retryMessage(w http.ResponseWriter, r *http.Request) {
	botID := BotID(r.PathValue("id"))
	bot, err := s.store.GetBot(botID)
	if err != nil {
		writeErr(w, writeStatus(err), err)
		return
	}
	msg, err := s.store.GetMessage(r.PathValue("messageId"))
	if err != nil {
		writeErr(w, writeStatus(err), err)
		return
	}
	if msg.BotID != botID || msg.Role != "user" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("message %s is not a user message of bot %s", msg.ID, botID))
		return
	}
	switch msg.Status {
	case StatusSent:
		writeErr(w, http.StatusConflict, fmt.Errorf("message %s was already answered", msg.ID))
		return
	case StatusAwaiting:
		writeErr(w, http.StatusConflict, fmt.Errorf("message %s is already being answered", msg.ID))
		return
	}
	seg, err := s.store.OpenSegment(botID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if msg.SegmentID != seg.ID {
		writeErr(w, http.StatusConflict,
			fmt.Errorf("message %s is in a sealed segment and is now history", msg.ID))
		return
	}
	if err := s.store.ClaimRetry(msg); err != nil {
		s.writeSendError(w, botID, err)
		return
	}
	msg.Status, msg.Error = StatusAwaiting, ""
	s.startTurn(bot, msg)
	writeJSON(w, http.StatusAccepted, msg)
}

// startTurn runs one model call off the request path. Nothing it learns goes
// back to the caller that triggered it — that response has already been written
// — so every outcome is recorded in the store, which is where the client reads
// from anyway.
func (s *Server) startTurn(bot Bot, msg Message) {
	s.turns.Add(1)
	go func() {
		defer s.turns.Done()
		// Deliberately not the request's context: that is cancelled the moment
		// the handler returns, which is now immediately. turnTimeout is the
		// backstop that keeps a wedged call from holding the bot's one in-flight
		// slot forever.
		ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
		defer cancel()
		if err := s.runTurn(ctx, bot, msg); err != nil {
			// Strand the turn with its reason. If even this fails the message
			// would stay awaiting until the next restart sweeps it, so it is
			// logged rather than silently dropped.
			if err := s.store.SetMessageStatus(msg.ID, StatusFailed, err.Error()); err != nil {
				log.Printf("botnet: could not settle failed turn %s: %v", msg.ID, err)
			}
		}
	}()
}

// runTurn calls the model for an already-persisted, awaiting user message and
// records the result.
//
// The prompt it assembles is the whole point of segments: the bot's system
// prompt, its memory blob, the ONE newest cumulative summary, and the open
// segment's raw messages. Sealed segments contribute only through that
// summary. The turn also carries the bot's toolbox, so the model can read and
// edit its memory mid-turn.
func (s *Server) runTurn(ctx context.Context, bot Bot, msg Message) error {
	if !bot.ModelValid {
		return unusableModel(bot)
	}
	seg, err := s.store.OpenSegment(bot.ID)
	if err != nil {
		return err
	}
	history, err := s.store.SegmentMessages(seg.ID)
	if err != nil {
		return err
	}
	summary, err := s.store.LatestSummary(bot.ID)
	if err != nil {
		return err
	}
	reply, err := s.llm.Complete(ctx, Prompt{
		Bot:      bot,
		Memory:   bot.Memory,
		Summary:  summary,
		Messages: history,
		Tools:    NewBotToolbox(s.store, bot.ID, s.search),
	})
	if err != nil {
		return err
	}
	// One transaction: the reply lands and the user turn settles together, so
	// the bot is never observably free with its reply missing. Any web sources
	// the model cited and the audit trail of every tool it called this turn are
	// stored on the reply.
	_, err = s.store.CompleteTurn(bot.ID, msg.ID, reply.Content, reply.Citations, reply.ToolCalls)
	return err
}

// writeSendError reports a refused send. ErrBusy carries the turn that is
// holding the bot, so the client can poll that message rather than guess what it
// collided with.
func (s *Server) writeSendError(w http.ResponseWriter, botID BotID, cause error) {
	if !errors.Is(cause, ErrBusy) {
		writeErr(w, writeStatus(cause), cause)
		return
	}
	body := map[string]any{"error": cause.Error()}
	if m, err := s.store.InFlight(botID); err == nil {
		body["message"] = m
	}
	writeJSON(w, http.StatusConflict, body)
}

// unusableModel is the failure a bot persisted with a since-removed model hits.
// It names the repair rather than surfacing an opaque upstream rejection.
func unusableModel(bot Bot) error {
	return fmt.Errorf("bot %s uses model %q, which is not in the model roster — "+
		"set a supported model with PATCH /v1/bots/%s", bot.ID, bot.Model, bot.ID)
}

// writeStatus maps a store error to its HTTP status; anything unrecognised is
// the server's fault, not the caller's.
func writeStatus(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrUnknownModel), errors.Is(err, ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrBusy), errors.Is(err, ErrIDConflict):
		return http.StatusConflict
	case errors.Is(err, ErrVersionMismatch):
		return http.StatusPreconditionFailed
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
