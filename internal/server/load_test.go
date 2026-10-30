package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/orestislef/agentic-chat/internal/auth"
	"github.com/orestislef/agentic-chat/internal/hub"
	"github.com/orestislef/agentic-chat/internal/store"
)

// TestLoad runs real agents over real sockets and reports throughput and latency. It takes
// a while, so it only runs on request:
//
//	AC_LOADTEST=1 go test -run TestLoad -v -timeout 10m ./internal/server
//
// AC_LOAD_AGENTS and AC_LOAD_SECONDS change its size.
func TestLoad(t *testing.T) {
	if os.Getenv("AC_LOADTEST") == "" {
		t.Skip("set AC_LOADTEST=1 to run")
	}
	agents := envInt("AC_LOAD_AGENTS", 100)
	seconds := envInt("AC_LOAD_SECONDS", 20)

	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "load.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	u, err := s.CreateAccount(ctx, "owner", "h", "", "team")
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.WorkspacesForUser(ctx, u.ID)
	ws := list[0].ID
	limits := hub.DefaultLimits()
	limits.MessagesPerMinute = 1 << 30
	cfg := Config{DataDir: dir, DownloadsDir: dir, Registration: RegistrationInvite, Limits: limits}
	srv := httptest.NewServer(New(cfg, s, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	defer srv.Close()

	tokens := make([]string, agents)
	for i := range tokens {
		tokens[i], _ = auth.NewToken("ac")
		if _, err := s.CreateAgent(ctx, ws, u.ID, "a"+strconv.Itoa(i), "", auth.HashToken(tokens[i])); err != nil {
			t.Fatal(err)
		}
	}

	transport := &http.Transport{MaxIdleConns: agents * 2, MaxIdleConnsPerHost: agents * 2}
	client := &http.Client{Transport: transport, Timeout: 60 * time.Second}

	// Everyone holds a live push connection and acknowledges what arrives.
	var sent sync.Map
	var mu sync.Mutex
	var sendLat, pushLat []time.Duration
	refused := 0
	var firstRefusal error
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var readers sync.WaitGroup
	for i := range tokens {
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/agent/ws", &websocket.DialOptions{
			HTTPClient: client,
			HTTPHeader: http.Header{"Authorization": {"Bearer " + tokens[i]}},
		})
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conn.SetReadLimit(1 << 20)
		readers.Add(1)
		go func() {
			defer readers.Done()
			defer conn.CloseNow()
			for {
				var frame struct {
					Type    string `json:"type"`
					Message struct {
						ID   int64  `json:"id"`
						Body string `json:"body"`
					} `json:"message"`
				}
				if err := wsjson.Read(runCtx, conn, &frame); err != nil {
					return
				}
				if at, ok := sent.Load(frame.Message.Body); ok {
					mu.Lock()
					pushLat = append(pushLat, time.Since(at.(time.Time)))
					mu.Unlock()
				}
				wsjson.Write(runCtx, conn, map[string]any{"type": "ack", "id": frame.Message.ID})
			}
		}()
	}
	time.Sleep(time.Second)

	// Everyone sends to the next agent as fast as the hub answers.
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	var senders sync.WaitGroup
	errs := make(chan error, agents)
	for i := range tokens {
		senders.Add(1)
		go func() {
			defer senders.Done()
			to := "a" + strconv.Itoa((i+1)%agents)
			for n := 0; time.Now().Before(deadline); n++ {
				body := fmt.Sprintf("load %d-%d with a realistic amount of text about pagination and caching", i, n)
				payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
					"name": "send_message", "arguments": map[string]any{"to": to, "message": body},
				}})
				req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader(payload))
				req.Header.Set("Authorization", "Bearer "+tokens[i])
				start := time.Now()
				sent.Store(body, start)
				resp, err := client.Do(req)
				if err != nil {
					// A burst can overflow the listen queue of this machine. Real clients retry.
					mu.Lock()
					refused++
					if firstRefusal == nil {
						firstRefusal = err
					}
					mu.Unlock()
					time.Sleep(50 * time.Millisecond)
					continue
				}
				data, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK || bytes.Contains(data, []byte(`"isError":true`)) {
					errs <- fmt.Errorf("status %d: %s", resp.StatusCode, data)
					return
				}
				mu.Lock()
				sendLat = append(sendLat, time.Since(start))
				mu.Unlock()
			}
		}()
	}
	senders.Wait()
	time.Sleep(2 * time.Second) // let pushes drain
	close(errs)
	for err := range errs {
		t.Errorf("send failed: %v", err)
		break
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	mu.Lock()
	defer mu.Unlock()
	t.Logf("%d agents for %d s: %d sends, %.0f per second, %d pushes received, %d connections refused", agents, seconds, len(sendLat), float64(len(sendLat))/float64(seconds), len(pushLat), refused)
	if firstRefusal != nil {
		t.Logf("first connection error: %v", firstRefusal)
	}
	t.Logf("send latency  %s", percentiles(sendLat))
	t.Logf("push latency  %s", percentiles(pushLat))
	t.Logf("goroutines %d, heap %d MiB, total from OS %d MiB", runtime.NumGoroutine(), m.HeapAlloc>>20, m.Sys>>20)
	if len(pushLat) < len(sendLat)*99/100 {
		t.Errorf("only %d of %d messages were pushed", len(pushLat), len(sendLat))
	}
	stop()
	readers.Wait()
}

func percentiles(d []time.Duration) string {
	if len(d) == 0 {
		return "no samples"
	}
	slices.Sort(d)
	at := func(p float64) time.Duration { return d[int(float64(len(d)-1)*p)].Round(100 * time.Microsecond) }
	return fmt.Sprintf("p50 %s  p95 %s  p99 %s  max %s", at(.5), at(.95), at(.99), d[len(d)-1].Round(time.Millisecond))
}

func envInt(name string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return fallback
}
