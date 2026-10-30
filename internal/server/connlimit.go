package server

import (
	"net/http"
	"sync"
)

// connLimiter caps the requests that can stay open for a long time: websockets, waiting
// inbox calls and tool calls. Each can hold a worker of a reverse proxy, and on a server
// shared with other sites, running out of those takes the other sites down too. Refused
// clients get a 503 and retry with backoff. A limit of zero turns all of this off.
type connLimiter struct {
	mu     sync.Mutex
	total  int
	perIP  int
	active int
	byIP   map[string]int
}

func newConnLimiter(total int) *connLimiter {
	// One address gets a tenth, so a single client cannot use up the whole hub.
	return &connLimiter{total: total, perIP: max(16, total/10), byIP: map[string]int{}}
}

func (l *connLimiter) acquire(ip string) bool {
	if l.total <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active >= l.total || l.byIP[ip] >= l.perIP {
		return false
	}
	l.active++
	l.byIP[ip]++
	return true
}

func (l *connLimiter) release(ip string) {
	if l.total <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if l.byIP[ip]--; l.byIP[ip] <= 0 {
		delete(l.byIP, ip)
	}
}

func (s *Server) limited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := s.clientIP(r)
		if !s.conns.acquire(ip) {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, "the hub is at its connection limit, try again shortly")
			return
		}
		defer s.conns.release(ip)
		next(w, r)
	}
}
