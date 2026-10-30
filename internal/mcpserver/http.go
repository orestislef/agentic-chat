// Package mcpserver is the hub's side of MCP: the /mcp endpoint and what the tools do.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/orestislef/agentic-chat/internal/auth"
	"github.com/orestislef/agentic-chat/internal/hub"
	"github.com/orestislef/agentic-chat/internal/mcp"
	"github.com/orestislef/agentic-chat/internal/store"
)

// maxRequestBytes leaves room for a full size message after JSON escaping.
const maxRequestBytes = 1 << 20

const maxKeyLength = 128

// Handler serves the Streamable HTTP transport in its stateless form: every POST
// carries a bearer token and gets one JSON answer. Nothing is kept per connection,
// so a hub restart costs clients nothing.
type Handler struct {
	tools     *toolbox
	store     *store.Store
	log       *slog.Logger
	responses *responseCache
}

// NewHandler creates the /mcp endpoint.
func NewHandler(s *store.Store, h *hub.Hub, log *slog.Logger) *Handler {
	return &Handler{tools: &toolbox{hub: h, store: s}, store: s, log: log, responses: newResponseCache()}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// No server initiated stream on this transport. Push goes through the bridge.
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agent, err := h.store.AgentByToken(r.Context(), auth.HashToken(mcp.BearerToken(r)))
	if errors.Is(err, store.ErrNotFound) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agentic-chat"`)
		http.Error(w, "invalid or missing token", http.StatusUnauthorized)
		return
	}
	if err != nil {
		// Saying 401 here would make clients give up on a token that is fine.
		h.log.Error("look up agent", "err", err)
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	if err := h.store.TouchAgent(r.Context(), agent.ID); err != nil {
		h.log.Warn("touch agent", "err", err)
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, mcp.Error(nil, mcp.CodeInvalidRequest, "request too large"))
		return
	}
	var req mcp.Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, mcp.Error(nil, mcp.CodeParseError, "invalid JSON"))
		return
	}
	if req.IsNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// A retry of a call we already answered gets the same answer back. Without this,
	// check_inbox would come back empty when its first response got lost on the way,
	// because the messages were already marked read.
	key := r.Header.Get(mcp.IdempotencyHeader)
	if len(key) > maxKeyLength {
		key = ""
	}
	ctx := r.Context()
	cacheable := key != "" && req.Method == "tools/call"
	if cacheable {
		if cached, ok := h.responses.get(agent.ID, key); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Write(cached)
			return
		}
		ctx = hub.WithIdempotencyKey(ctx, key)
	}
	resp := h.dispatch(ctx, agent, &req)
	if ctx.Err() != nil {
		// The hub is shutting down or the client left. Nothing was answered, so this
		// must read as "try again" and not as a tool failure the model has to handle.
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, mcp.Error(req.ID, mcp.CodeInternalError, "could not encode result"))
		return
	}
	if cacheable {
		h.responses.put(agent.ID, key, encoded)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(encoded)
}

func (h *Handler) dispatch(ctx context.Context, agent *store.Agent, req *mcp.Request) *mcp.Response {
	switch req.Method {
	case "initialize":
		caps := map[string]any{"tools": map[string]any{}}
		return mcp.Result(req.ID, mcp.InitializeResult(req.Params, caps, mcp.SharedInstructions+mcp.PollInstructions))
	case "ping":
		return mcp.Result(req.ID, map[string]any{})
	case "tools/list":
		return mcp.Result(req.ID, map[string]any{"tools": mcp.Tools()})
	case "tools/call":
		return h.callTool(ctx, agent, req)
	}
	return mcp.Error(req.ID, mcp.CodeMethodNotFound, "method not found: "+req.Method)
}

func (h *Handler) callTool(ctx context.Context, agent *store.Agent, req *mcp.Request) *mcp.Response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return mcp.Error(req.ID, mcp.CodeInvalidParams, "invalid params")
	}
	out, err := h.tools.call(ctx, agent, params.Name, params.Arguments)
	if errors.Is(err, errUnknownTool) {
		return mcp.Error(req.ID, mcp.CodeInvalidParams, "unknown tool: "+params.Name)
	}
	if err != nil {
		return mcp.Result(req.ID, mcp.TextResult(err.Error(), true))
	}
	text, err := json.Marshal(out)
	if err != nil {
		return mcp.Error(req.ID, mcp.CodeInternalError, "could not encode result")
	}
	return mcp.Result(req.ID, mcp.TextResult(string(text), false))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// responseCache remembers recent tool answers for a few minutes, long enough to
// cover the retries of a client whose connection dropped.
type responseCache struct {
	mu      sync.Mutex
	entries map[string]cachedResponse
}

type cachedResponse struct {
	body    []byte
	expires time.Time
}

const (
	responseCacheTTL  = 5 * time.Minute
	responseCacheSize = 2048
)

func newResponseCache() *responseCache {
	return &responseCache{entries: map[string]cachedResponse{}}
}

func cacheKey(agentID int64, key string) string {
	return strconv.FormatInt(agentID, 10) + ":" + key
}

func (c *responseCache) get(agentID int64, key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[cacheKey(agentID, key)]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.body, true
}

func (c *responseCache) put(agentID int64, key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if len(c.entries) >= responseCacheSize {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
		// Everything still fresh means unusual traffic. Losing the retry protection
		// for a moment is better than growing without bound.
		if len(c.entries) >= responseCacheSize {
			c.entries = map[string]cachedResponse{}
		}
	}
	c.entries[cacheKey(agentID, key)] = cachedResponse{body: body, expires: now.Add(responseCacheTTL)}
}
