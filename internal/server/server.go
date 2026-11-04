// Package server is the hub: the MCP endpoint chats connect to, and the web pages for rooms.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orestislef/agentic-chat/internal/store"
	"github.com/orestislef/agentic-chat/web"
)

// Config is read from the environment.
type Config struct {
	Addr    string
	DataDir string
	// TrustProxy reads https and client addresses from a reverse proxy's headers.
	TrustProxy bool
	// MaxConnections caps requests that stay open a long time, zero means no cap.
	MaxConnections int
}

// ConfigFromEnv reads the AC_* variables.
func ConfigFromEnv() (Config, error) {
	c := Config{Addr: env("AC_ADDR", ":8080"), DataDir: env("AC_DATA_DIR", "./data")}
	var err error
	if c.TrustProxy, err = strconv.ParseBool(env("AC_TRUST_PROXY", "false")); err != nil {
		return c, fmt.Errorf("AC_TRUST_PROXY must be true or false")
	}
	if c.MaxConnections, err = strconv.Atoi(env("AC_MAX_CONNECTIONS", "0")); err != nil || c.MaxConnections < 0 {
		return c, fmt.Errorf("AC_MAX_CONNECTIONS must be a number, 0 or more")
	}
	return c, nil
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// DatabasePath is where the SQLite file lives.
func (c Config) DatabasePath() string {
	return filepath.Join(c.DataDir, "rooms.db")
}

// Limits that keep one room or one address from wearing out the hub.
const (
	maxMessageBytes   = 64 * 1024
	messagesPerMinute = 120
	roomsPerHour      = 20
	maxWait           = 110 * time.Second // stays under the point where clients move a call to the background
)

// Server holds what the handlers share.
type Server struct {
	cfg     Config
	store   *store.Store
	log     *slog.Logger
	static  *staticHandler
	conns   *connLimiter
	waiters *waiters
	sends   *windowLimiter
	creates *windowLimiter
}

// New creates a server on an open store.
func New(cfg Config, st *store.Store, log *slog.Logger) *Server {
	return &Server{
		cfg:     cfg,
		store:   st,
		log:     log,
		static:  newStaticHandler(web.FS),
		conns:   newConnLimiter(cfg.MaxConnections),
		waiters: newWaiters(),
		sends:   newWindowLimiter(messagesPerMinute, time.Minute),
		creates: newWindowLimiter(roomsPerHour, time.Hour),
	}
}

// Handler builds the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("/mcp", s.limited(s.handleMCP))
	mux.HandleFunc("POST /api/rooms", s.handleCreateRoom)
	mux.HandleFunc("GET /api/rooms/{token}", s.handleRoom)
	mux.HandleFunc("DELETE /api/rooms/{token}", s.handleDeleteRoom)
	mux.HandleFunc("DELETE /api/rooms/{token}/members/{name}", s.handleRemoveMember)
	mux.HandleFunc("GET /r/{token}", func(w http.ResponseWriter, r *http.Request) { s.static.serveFile(w, r, "index.html") })
	mux.Handle("/", s.static)
	return secure(mux)
}

// Run serves until ctx ends, and cleans up the database once an hour.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Waiting calls end when the hub stops, instead of holding up the shutdown.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go s.cleanLoop(ctx)
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	s.log.Info("listening", "addr", s.cfg.Addr)
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func (s *Server) cleanLoop(ctx context.Context) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		if err := s.store.Clean(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("clean up", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok\n"))
}

// secure adds headers that keep the pages from being framed, and room links from leaking
// to other sites through the referrer.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// Room links carry 128 random bits. Only their hash is stored, so a copy of the database
// does not let anyone into a room.
func newToken(bytes int) string {
	buf := make([]byte, bytes)
	rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

var tokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// roomByToken finds a room, treating a malformed token like an unknown one.
func (s *Server) roomByToken(ctx context.Context, token string) (*store.Room, error) {
	if !tokenRE.MatchString(token) {
		return nil, store.ErrNotFound
	}
	return s.store.RoomByToken(ctx, hash(token))
}

var errTooManyRooms = errors.New("too many new rooms from this address, try again in an hour")

// createRoom makes a room and returns its link token.
func (s *Server) createRoom(r *http.Request) (*store.Room, string, error) {
	if !s.creates.allow(s.clientIP(r)) {
		return nil, "", errTooManyRooms
	}
	token := newToken(16)
	room, err := s.store.CreateRoom(r.Context(), hash(token))
	return room, token, err
}

// CleanName turns something like a folder name into a member name: lowercase letters, digits,
// - and _, up to 32 characters.
func CleanName(raw string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		if b.Len() >= 32 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.Trim(b.String(), "-_")
	if name == "everyone" {
		return ""
	}
	return name
}

// state says whether a member is around. A joined chat checks in every 2 minutes.
func state(m store.Member) string {
	if time.Since(time.UnixMilli(m.SeenAt)) < 3*time.Minute {
		return "online"
	}
	return "away"
}

// clientIP is the address used for limits. Behind a proxy the last X-Forwarded-For entry is
// the one the proxy added, earlier ones are whatever the client claimed.
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

var hostRE = regexp.MustCompile(`^[A-Za-z0-9.\-:\[\]]+$`)

// baseURL is the address people reach this hub on, for the links it hands out.
func (s *Server) baseURL(r *http.Request) string {
	host := r.Host
	if !hostRE.MatchString(host) {
		host = "localhost"
	}
	scheme := "http"
	if r.TLS != nil || s.cfg.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + host
}

// invite is the message to send someone so their chats can join a room.
func invite(base, link string) string {
	return "Join my agentic-chat room, so our Claude Code chats can talk to each other.\n\n" +
		"1. Once, in a terminal:\n" +
		"claude mcp add --transport http -s user agentic-chat " + base + "/mcp\n\n" +
		"2. Restart Claude Code, then tell your chat:\n" +
		"join the room " + link + " as backend\n\n" +
		"Use a name that fits your project instead of backend. The link is the only key to the room, so keep it between us."
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "something went wrong, try again")
}

// waiters wakes up calls that wait for new messages in a room.
type waiters struct {
	mu    sync.Mutex
	rooms map[int64]chan struct{}
}

func newWaiters() *waiters {
	return &waiters{rooms: map[int64]chan struct{}{}}
}

// wait returns a channel that is closed on the next message in the room. Take it before
// looking for messages, so one that arrives in between is not missed.
func (w *waiters) wait(roomID int64) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch, ok := w.rooms[roomID]
	if !ok {
		ch = make(chan struct{})
		w.rooms[roomID] = ch
	}
	return ch
}

func (w *waiters) wake(roomID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ch, ok := w.rooms[roomID]; ok {
		close(ch)
		delete(w.rooms, roomID)
	}
}

// windowLimiter allows a number of events per key in a fixed window.
type windowLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	seen   map[string]*window
}

type window struct {
	start time.Time
	count int
}

func newWindowLimiter(limit int, d time.Duration) *windowLimiter {
	return &windowLimiter{max: limit, window: d, seen: map[string]*window{}}
}

func (l *windowLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := time.Now()
	if len(l.seen) > 10000 {
		for k, w := range l.seen {
			if t.Sub(w.start) > l.window {
				delete(l.seen, k)
			}
		}
	}
	w := l.seen[key]
	if w == nil || t.Sub(w.start) > l.window {
		w = &window{start: t}
		l.seen[key] = w
	}
	if w.count >= l.max {
		return false
	}
	w.count++
	return true
}
