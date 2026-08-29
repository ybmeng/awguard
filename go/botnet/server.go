package botnet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	modelselector "stdtools/go/lib/modelSelector"
)

// Server is the botnet state owner: it holds the single Store and the LLM, and
// exposes the HTTP API the UI calls. Every state change (create bot, send
// message) goes through here — clients keep no state of their own.
type Server struct {
	store   *Store
	llm     LLM
	netID   string
	keyPath string // where SetKey persists the OpenRouter key; "" disables persistence
}

// ConfigureKeyPersistence tells the server where to save a key set via the
// config endpoint, so it survives a restart.
func (s *Server) ConfigureKeyPersistence(path string) { s.keyPath = path }

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
	mux.HandleFunc("GET /v1/bots", s.listBots)
	mux.HandleFunc("POST /v1/bots", s.createBot)
	mux.HandleFunc("DELETE /v1/bots/{id}", s.deleteBot)
	mux.HandleFunc("GET /v1/bots/{id}/messages", s.getMessages)
	mux.HandleFunc("POST /v1/bots/{id}/messages", s.sendMessage)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, modelselector.All())
}

func (s *Server) listBots(w http.ResponseWriter, _ *http.Request) {
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
		writeErr(w, http.StatusInternalServerError, err)
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

func (s *Server) getMessages(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.store.Conversation(BotID(r.PathValue("id")))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if msgs == nil {
		msgs = []Message{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

// sendMessage is the chat lifecycle, server-side and no compaction:
// append the user message, call the LLM with the FULL history, append the
// reply, return the updated conversation. On LLM failure the user message
// stays persisted and the error surfaces — retrying just sends again.
func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	botID := BotID(r.PathValue("id"))
	var in struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	bot, err := s.store.GetBot(botID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if _, err := s.store.AppendMessage(botID, "user", in.Content); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	history, err := s.store.Conversation(botID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	reply, err := s.llm.Complete(r.Context(), bot, history)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if _, err := s.store.AppendMessage(botID, "bot", reply); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	msgs, err := s.store.Conversation(botID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
