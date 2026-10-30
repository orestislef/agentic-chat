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
	"strings"
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

// ConnectionTokenPrefix marks tokens from the browser sign in. Each chat behind one of them
// gets its own agent. Tokens without it belong to exactly one agent.
const ConnectionTokenPrefix = "acc_"

// sessionHeader is how a chat identifies itself on each request.
const sessionHeader = "Mcp-Session-Id"

// Handler serves the Streamable HTTP transport: every POST carries a bearer token and gets
// one JSON answer. Sessions only say which chat is calling, and live in the database, so a
// hub restart costs clients nothing.
type Handler struct {
	tools     *toolbox
	store     *store.Store
	log       *slog.Logger
	responses *responseCache

	// ResourceMetadataURL tells clients where to start signing in. Optional.
	ResourceMetadataURL func(*http.Request) string
	// AgentLimit caps agents a workspace can create through join, 0 means no cap. Optional.
	AgentLimit func(ctx context.Context, workspaceID int64) int
}

// caller is who a request comes from. A chat that has not joined yet has no agent.
type caller struct {
	agent       *store.Agent
	conn        *store.Connection
	sessionHash string
	removed     bool
	wanted      string
}

// NewHandler creates the /mcp endpoint.
func NewHandler(s *store.Store, h *hub.Hub, log *slog.Logger) *Handler {
	return &Handler{tools: &toolbox{hub: h, store: s}, store: s, log: log, responses: newResponseCache()}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		// No server initiated stream on this transport. Push goes through the bridge.
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, err := h.authenticate(r)
	if errors.Is(err, store.ErrNotFound) {
		challenge := `Bearer realm="agentic-chat"`
		if h.ResourceMetadataURL != nil {
			challenge = `Bearer resource_metadata="` + h.ResourceMetadataURL(r) + `"`
		}
		w.Header().Set("WWW-Authenticate", challenge)
		http.Error(w, "invalid or missing token", http.StatusUnauthorized)
		return
	}
	if err != nil {
		// Saying 401 here would make clients give up on a token that is fine.
		h.log.Error("look up caller", "err", err)
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodDelete {
		if c.conn != nil {
			if err := h.store.EndSession(r.Context(), c.sessionHash); err != nil {
				h.log.Warn("end session", "err", err)
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if c.agent != nil {
		if err := h.store.TouchAgent(r.Context(), c.agent.ID); err != nil {
			h.log.Warn("touch agent", "err", err)
		}
	}
	if c.conn != nil {
		if err := h.store.TouchSession(r.Context(), c.sessionHash); err != nil {
			h.log.Warn("touch session", "err", err)
		}
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
	if c.conn != nil && req.Method == "initialize" {
		id, err := auth.NewToken("s")
		if err != nil {
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set(sessionHeader, id)
	}

	// A retry of a call we already answered gets the same answer back. Without this,
	// check_inbox would come back empty when its first response got lost on the way,
	// because the messages were already marked read.
	key := r.Header.Get(mcp.IdempotencyHeader)
	if len(key) > maxKeyLength {
		key = ""
	}
	ctx := r.Context()
	cacheable := key != "" && req.Method == "tools/call" && c.agent != nil
	if cacheable {
		if cached, ok := h.responses.get(c.agent.ID, key); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Write(cached)
			return
		}
		ctx = hub.WithIdempotencyKey(ctx, key)
	}
	resp := h.dispatch(ctx, c, &req)
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
		h.responses.put(c.agent.ID, key, encoded)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(encoded)
}

func (h *Handler) authenticate(r *http.Request) (*caller, error) {
	token := mcp.BearerToken(r)
	if !strings.HasPrefix(token, ConnectionTokenPrefix) {
		agent, err := h.store.AgentByToken(r.Context(), auth.HashToken(token))
		if err != nil {
			return nil, err
		}
		return &caller{agent: agent}, nil
	}
	conn, err := h.store.ConnectionByToken(r.Context(), auth.HashToken(token))
	if err != nil {
		return nil, err
	}
	c := &caller{conn: conn, sessionHash: auth.HashToken(strconv.FormatInt(conn.ID, 10) + ":" + r.Header.Get(sessionHeader))}
	state, err := h.store.SessionAgent(r.Context(), c.sessionHash)
	if err != nil {
		return nil, err
	}
	c.agent, c.removed, c.wanted = state.Agent, state.Removed, state.Wanted
	return c, nil
}

// settleName moves a chat to the name it asked for once that is free. A closed chat gives no
// notice, so a project reopened right away starts as backend-2 while backend looks busy. Left
// alone, its messages would keep landing on a name nobody reads anymore.
func (h *Handler) settleName(ctx context.Context, c *caller) string {
	if c.agent == nil || c.wanted == "" || c.agent.Name == c.wanted {
		return ""
	}
	settled, _, err := h.joinAs(ctx, c, c.wanted)
	if err != nil || settled.ID == c.agent.ID {
		return ""
	}
	if err := h.store.RetireChatAgent(ctx, c.agent, settled); err != nil {
		h.log.Warn("retire stand-in agent", "err", err)
	}
	h.tools.hub.AgentsChanged(c.conn.WorkspaceID)
	c.agent = settled
	return "This chat is now " + settled.Name + ", that name came free."
}

func (h *Handler) dispatch(ctx context.Context, c *caller, req *mcp.Request) *mcp.Response {
	switch req.Method {
	case "initialize":
		caps := map[string]any{"tools": map[string]any{}}
		instructions := mcp.SharedInstructions + mcp.PollInstructions
		if c.conn != nil {
			instructions = mcp.SharedInstructions + mcp.ChatInstructions
		}
		return mcp.Result(req.ID, mcp.InitializeResult(req.Params, caps, instructions))
	case "ping":
		return mcp.Result(req.ID, map[string]any{})
	case "tools/list":
		tools := mcp.Tools()
		if c.conn != nil {
			tools = append([]mcp.Tool{mcp.JoinTool()}, tools...)
		}
		return mcp.Result(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		return h.callTool(ctx, c, req)
	}
	return mcp.Error(req.ID, mcp.CodeMethodNotFound, "method not found: "+req.Method)
}

func (h *Handler) callTool(ctx context.Context, c *caller, req *mcp.Request) *mcp.Response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return mcp.Error(req.ID, mcp.CodeInvalidParams, "invalid params")
	}
	var out any
	var err error
	renamed := ""
	if c.conn != nil && params.Name != "join" {
		renamed = h.settleName(ctx, c)
	}
	switch {
	case params.Name == "join" && c.conn != nil:
		out, err = h.join(ctx, c, params.Arguments)
	case c.removed:
		err = errors.New("this chat's agent was removed in the web UI. Tell your user. If they want this chat back, call join")
	default:
		if c.agent == nil && c.conn != nil {
			// A chat that skipped join still works, under a name it can change later.
			if c.agent, _, err = h.joinAs(ctx, c, "chat"); err != nil {
				break
			}
		}
		out, err = h.tools.call(ctx, c.agent, params.Name, params.Arguments)
	}
	if errors.Is(err, errUnknownTool) {
		return mcp.Error(req.ID, mcp.CodeInvalidParams, "unknown tool: "+params.Name)
	}
	if err != nil {
		return mcp.Result(req.ID, mcp.TextResult(err.Error(), true))
	}
	if c.conn != nil && c.agent != nil && params.Name != "check_inbox" {
		// Without push this is the only way a working chat learns something arrived.
		if m, ok := out.(map[string]any); ok {
			if n, err := h.store.CountUnread(ctx, c.agent.ID); err == nil && n > 0 {
				m["unread_messages"] = n
			}
			if renamed != "" {
				m["name_changed"] = renamed
			}
		}
	}
	text, err := json.Marshal(out)
	if err != nil {
		return mcp.Error(req.ID, mcp.CodeInternalError, "could not encode result")
	}
	return mcp.Result(req.ID, mcp.TextResult(string(text), false))
}

func (h *Handler) join(ctx context.Context, c *caller, args json.RawMessage) (any, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	name := CleanName(in.Name)
	if name == "" {
		return nil, errors.New("name needs at least one letter or digit, for example the name of your project folder")
	}
	agent, created, err := h.joinAs(ctx, c, name)
	if err != nil {
		return nil, err
	}
	c.agent = agent
	result := map[string]any{"you": agent.Name, "new": created}
	if agent.Name != name {
		result["note"] = name + " is in use by another chat, so this chat is " + agent.Name + "."
	}
	return result, nil
}

func (h *Handler) joinAs(ctx context.Context, c *caller, name string) (*store.Agent, bool, error) {
	limit := 0
	if h.AgentLimit != nil {
		limit = h.AgentLimit(ctx, c.conn.WorkspaceID)
	}
	agent, created, err := h.store.JoinAgent(ctx, c.conn, c.sessionHash, name, limit)
	if err != nil {
		return nil, false, err
	}
	if created {
		h.tools.hub.AgentsChanged(c.conn.WorkspaceID)
	}
	return agent, created, nil
}

// CleanName turns something like a folder name into a valid agent name.
func CleanName(raw string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 32 {
			break
		}
	}
	return strings.Trim(b.String(), "-_")
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
