// Package server exposes the hub over HTTP: web UI, JSON API, websockets and the MCP endpoint.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orestislef/agentic-chat/internal/auth"
	"github.com/orestislef/agentic-chat/internal/hub"
	"github.com/orestislef/agentic-chat/internal/mcp"
	"github.com/orestislef/agentic-chat/internal/store"
	"github.com/orestislef/agentic-chat/web"
)

const (
	sessionCookie   = "ac_session"
	sessionLifetime = 30 * 24 * time.Hour
	maxJSONBody     = 1 << 20
)

// Server holds the shared dependencies of all handlers.
type Server struct {
	cfg     Config
	store   *store.Store
	hub     *hub.Hub
	log     *slog.Logger
	limits  *attemptLimiter
	signups *attemptLimiter
}

// New creates a server.
func New(cfg Config, s *store.Store, log *slog.Logger) *Server {
	srv := &Server{
		cfg:     cfg,
		store:   s,
		hub:     hub.New(s, cfg.Limits, log),
		log:     log,
		limits:  newAttemptLimiter(10, 15*time.Minute),
		signups: newAttemptLimiter(demoSignupsPerIP, 24*time.Hour),
	}
	if cfg.Demo {
		s.SetDemo(true)
		srv.hub.SetQuota(srv.demoQuota)
	}
	return srv
}

// Handler builds the route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("/mcp", mcp.NewHandler(s.store, s.hub, s.log))

	api := http.NewServeMux()
	api.HandleFunc("GET /api/config", s.handleConfig)
	api.HandleFunc("POST /api/auth/register", s.handleRegister)
	api.HandleFunc("POST /api/auth/login", s.handleLogin)
	api.HandleFunc("POST /api/auth/logout", s.handleLogout)
	api.HandleFunc("GET /api/me", s.withUser(s.handleMe))
	api.HandleFunc("POST /api/me/password", s.withUser(s.handleChangePassword))

	api.HandleFunc("GET /api/workspaces", s.withUser(s.handleListWorkspaces))
	api.HandleFunc("POST /api/workspaces", s.withUser(s.handleCreateWorkspace))
	api.HandleFunc("POST /api/invites/accept", s.withUser(s.handleAcceptInvite))

	api.HandleFunc("GET /api/workspaces/{id}/members", s.withMember(s.handleListMembers))
	api.HandleFunc("DELETE /api/workspaces/{id}/members/{userID}", s.withMember(s.handleRemoveMember))
	api.HandleFunc("GET /api/workspaces/{id}/invites", s.withOwner(s.handleListInvites))
	api.HandleFunc("POST /api/workspaces/{id}/invites", s.withOwner(s.handleCreateInvite))
	api.HandleFunc("DELETE /api/workspaces/{id}/invites/{inviteID}", s.withOwner(s.handleDeleteInvite))

	api.HandleFunc("GET /api/workspaces/{id}/agents", s.withMember(s.handleListAgents))
	api.HandleFunc("POST /api/workspaces/{id}/agents", s.withMember(s.handleCreateAgent))
	api.HandleFunc("DELETE /api/workspaces/{id}/agents/{agentID}", s.withMember(s.handleRevokeAgent))
	api.HandleFunc("POST /api/workspaces/{id}/agents/{agentID}/rotate", s.withMember(s.handleRotateAgent))

	api.HandleFunc("GET /api/workspaces/{id}/threads", s.withMember(s.handleListThreads))
	api.HandleFunc("GET /api/workspaces/{id}/messages", s.withMember(s.handleListMessages))
	api.HandleFunc("POST /api/workspaces/{id}/messages", s.withMember(s.handleSendMessage))
	api.HandleFunc("GET /api/workspaces/{id}/ws", s.withMember(s.handleWorkspaceSocket))

	api.HandleFunc("GET /api/agent/me", s.withAgent(s.handleAgentMe))
	api.HandleFunc("GET /api/agent/inbox", s.withAgent(s.handleAgentInbox))
	api.HandleFunc("POST /api/agent/inbox/ack", s.withAgent(s.handleAgentInboxAck))
	api.HandleFunc("GET /api/agent/ws", s.withAgent(s.handleAgentSocket))

	// Browsers cannot send the agent bearer header cross origin without a preflight
	// we never allow, so this only has to protect the cookie based routes.
	mux.Handle("/api/", http.NewCrossOriginProtection().Handler(api))

	mux.HandleFunc("GET /install.sh", s.handleInstallShell)
	mux.HandleFunc("GET /install.ps1", s.handleInstallPowerShell)
	mux.HandleFunc("GET /downloads/{file}", s.handleDownload)
	mux.Handle("/", http.FileServerFS(web.FS))

	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; connect-src 'self' ws: wss:; img-src 'self' data:; "+
				"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// Run serves until ctx ends, then shuts down cleanly.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      130 * time.Second, // longer than the longest blocking tool call
		IdleTimeout:       2 * time.Minute,
		// Ends waiting calls at shutdown so they can answer "try again" in time.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go s.maintenance(ctx)

	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	s.log.Info("listening", "addr", s.cfg.Addr, "registration", s.cfg.Registration, "demo", s.cfg.Demo)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// maintenance cleans up once at start and then every hour.
func (s *Server) maintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if s.cfg.RetentionDays > 0 {
			cutoff := time.Now().AddDate(0, 0, -s.cfg.RetentionDays).UnixMilli()
			n, err := s.store.PruneMessages(ctx, cutoff)
			if err != nil && ctx.Err() == nil {
				s.log.Warn("prune messages", "err", err)
			} else if n > 0 {
				s.log.Info("pruned old messages", "count", n)
			}
		}
		if s.cfg.Demo {
			s.pruneDemo(ctx)
		}
		if err := s.store.Maintain(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("database maintenance", "err", err)
		}
		if s.cfg.BackupKeep > 0 {
			if err := s.dailyBackup(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("daily backup failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// dailyBackup writes one copy per day and removes the oldest beyond the keep count.
func (s *Server) dailyBackup(ctx context.Context) error {
	dir := filepath.Join(s.cfg.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "agentic-chat-"+time.Now().UTC().Format("2006-01-02")+".db")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := s.store.Backup(ctx, path); err != nil {
		return err
	}
	s.log.Info("wrote backup", "path", path)
	existing, err := filepath.Glob(filepath.Join(dir, "agentic-chat-*.db"))
	if err != nil {
		return err
	}
	sort.Strings(existing) // the date in the name sorts oldest first
	for len(existing) > s.cfg.BackupKeep {
		if err := os.Remove(existing[0]); err != nil {
			return err
		}
		existing = existing[1:]
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Auth wrappers.

type userHandler func(w http.ResponseWriter, r *http.Request, u *store.User)

type memberHandler func(w http.ResponseWriter, r *http.Request, c *memberContext)

type agentHandler func(w http.ResponseWriter, r *http.Request, a *store.Agent)

// memberContext is the caller of a workspace route.
type memberContext struct {
	User        *store.User
	WorkspaceID int64
	Role        string
}

func (s *Server) withUser(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "log in first")
			return
		}
		u, err := s.store.UserBySession(r.Context(), auth.HashToken(cookie.Value))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "log in first")
			return
		}
		next(w, r, u)
	}
}

func (s *Server) withMember(next memberHandler) http.HandlerFunc {
	return s.withUser(func(w http.ResponseWriter, r *http.Request, u *store.User) {
		id, ok := pathID(r, "id")
		if !ok {
			writeError(w, http.StatusNotFound, "workspace not found")
			return
		}
		role, err := s.store.MemberRole(r.Context(), id, u.ID)
		if err != nil {
			// Same answer for "does not exist" and "not yours".
			writeError(w, http.StatusNotFound, "workspace not found")
			return
		}
		next(w, r, &memberContext{User: u, WorkspaceID: id, Role: role})
	})
}

func (s *Server) withOwner(next memberHandler) http.HandlerFunc {
	return s.withMember(func(w http.ResponseWriter, r *http.Request, c *memberContext) {
		if c.Role != "owner" {
			writeError(w, http.StatusForbidden, "only a workspace owner can do this")
			return
		}
		next(w, r, c)
	})
}

func (s *Server) withAgent(next agentHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, err := s.store.AgentByToken(r.Context(), auth.HashToken(mcp.BearerToken(r)))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="agentic-chat"`)
			writeError(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		if err := s.store.TouchAgent(r.Context(), a.ID); err != nil {
			s.log.Warn("touch agent", "err", err)
		}
		next(w, r, a)
	}
}

// Helpers.

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// internalError logs the cause and tells the client nothing about it.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		writeError(w, http.StatusServiceUnavailable, "try again")
		return
	}
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "something went wrong on the server, check its logs")
}

func readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "send application/json")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request too large")
		return false
	}
	if err := json.Unmarshal(body, into); err != nil {
		writeError(w, http.StatusBadRequest, "request body is not valid JSON")
		return false
	}
	return true
}

func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil && id > 0
}

func queryInt(r *http.Request, name string, fallback, lo, hi int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return fallback
	}
	return max(lo, min(n, hi))
}

// clientIP returns the address used for rate limiting. Behind a proxy the last
// X-Forwarded-For entry is the one the proxy itself added, earlier entries are
// whatever the client claimed.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			parts := strings.Split(fwd, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return s.cfg.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

var hostRE = regexp.MustCompile(`^[A-Za-z0-9.\-:\[\]]+$`)

// baseURL is the address clients reach this hub on, used in generated scripts.
func (s *Server) baseURL(r *http.Request) (string, bool) {
	if !hostRE.MatchString(r.Host) {
		return "", false
	}
	scheme := "http"
	if s.isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host, true
}

// attemptLimiter counts failed credential attempts per key in a fixed window.
type attemptLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	seen   map[string]*attemptWindow
}

type attemptWindow struct {
	start time.Time
	count int
}

func newAttemptLimiter(limit int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{max: limit, window: window, seen: map[string]*attemptWindow{}}
}

// Allow reports whether another attempt is allowed and records it.
func (l *attemptLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.seen) > 10000 {
		for k, w := range l.seen {
			if now.Sub(w.start) >= l.window {
				delete(l.seen, k)
			}
		}
	}
	w := l.seen[key]
	if w == nil || now.Sub(w.start) >= l.window {
		l.seen[key] = &attemptWindow{start: now, count: 1}
		return true
	}
	w.count++
	return w.count <= l.max
}

// Reset clears the counter after a successful login.
func (l *attemptLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.seen, key)
}
