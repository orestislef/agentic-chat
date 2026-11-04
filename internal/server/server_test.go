package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orestislef/agentic-chat/internal/store"
)

type hub struct {
	t   *testing.T
	url string
	srv *Server
}

func start(t *testing.T) *hub {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "rooms.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); st.Close() })
	return &hub{t: t, url: ts.URL, srv: s}
}

// chat is one Claude Code session talking to the hub.
type chat struct {
	t       *testing.T
	url     string
	session string
	next    int
}

func (h *hub) chat() *chat {
	h.t.Helper()
	c := &chat{t: h.t, url: h.url}
	resp, out := c.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("initialize: %d", resp.StatusCode)
	}
	c.session = resp.Header.Get("Mcp-Session-Id")
	if c.session == "" {
		h.t.Fatal("no session")
	}
	if !strings.Contains(out["result"].(map[string]any)["instructions"].(string), "CronCreate") {
		h.t.Fatal("instructions miss the background check")
	}
	return c
}

func (c *chat) rpc(method string, params any) (*http.Response, map[string]any) {
	c.t.Helper()
	c.next++
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, c.url+"/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// call runs a tool and returns its result, and whether the tool said it failed.
func (c *chat) call(tool string, args map[string]any) (map[string]any, string) {
	c.t.Helper()
	_, out := c.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
	result, ok := out["result"].(map[string]any)
	if !ok {
		c.t.Fatalf("%s: %v", tool, out)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if result["isError"] == true {
		return nil, text
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		c.t.Fatalf("%s: %s", tool, text)
	}
	return data, ""
}

func (c *chat) must(tool string, args map[string]any) map[string]any {
	c.t.Helper()
	out, failed := c.call(tool, args)
	if failed != "" {
		c.t.Fatalf("%s failed: %s", tool, failed)
	}
	return out
}

func messages(out map[string]any) []map[string]any {
	list := []map[string]any{}
	for _, m := range out["messages"].([]any) {
		list = append(list, m.(map[string]any))
	}
	return list
}

func TestCreateRoomSendTheLinkAndTalk(t *testing.T) {
	h := start(t)
	mine := h.chat()
	created := mine.must("create_room", map[string]any{"name": "Driver App"})
	link := created["room"].(string)
	if created["you"] != "driver-app" || !strings.HasPrefix(link, h.url+"/r/") {
		t.Fatalf("%v", created)
	}
	if invite := created["invite"].(string); !strings.Contains(invite, link) || !strings.Contains(invite, h.url+"/mcp") {
		t.Fatalf("invite misses the link or the install command: %s", invite)
	}

	theirs := h.chat()
	joined := theirs.must("join_room", map[string]any{"link": "  " + link + "/ ", "name": "callcenter"})
	if joined["you"] != "callcenter" || len(joined["members"].([]any)) != 2 {
		t.Fatalf("%v", joined)
	}

	theirs.must("send_message", map[string]any{"to": "driver-app", "message": "which endpoint sends the call?"})
	theirs.must("send_message", map[string]any{"message": "server restart in 5 minutes"})
	listed := mine.must("list_members", nil)
	if listed["unread"] != float64(2) || listed["unread_for_you"] != float64(1) {
		t.Fatalf("no hint about waiting messages: %v", listed)
	}
	got := messages(mine.must("read_messages", nil))
	if len(got) != 2 || got[0]["from"] != "callcenter" || got[0]["to"] != "driver-app" || got[1]["to"] != nil {
		t.Fatalf("%v", got)
	}
	if again := messages(mine.must("read_messages", nil)); len(again) != 0 {
		t.Fatalf("read twice: %v", again)
	}
	// Own messages never come back as unread.
	mine.must("send_message", map[string]any{"to": "callcenter", "message": "POST /calls"})
	if own := messages(mine.must("read_messages", nil)); len(own) != 0 {
		t.Fatalf("%v", own)
	}
	if got := messages(theirs.must("read_messages", nil)); len(got) != 1 || got[0]["text"] != "POST /calls" {
		t.Fatalf("%v", got)
	}
	history := messages(theirs.must("get_history", nil))
	if len(history) != 3 {
		t.Fatalf("%v", history)
	}
	if found := messages(theirs.must("get_history", map[string]any{"search": "restart"})); len(found) != 1 {
		t.Fatalf("%v", found)
	}
}

func TestRestartKeepsTheIdentityAndWhatWasWaiting(t *testing.T) {
	h := start(t)
	app := h.chat()
	link := app.must("create_room", map[string]any{"name": "app"})["room"].(string)
	backend := h.chat()
	backend.must("join_room", map[string]any{"link": link, "name": "backend"})
	app.must("send_message", map[string]any{"to": "backend", "message": "are you there?"})

	// Claude Code restarts: a new session that knows nothing yet.
	restarted := h.chat()
	if _, failed := restarted.call("read_messages", nil); !strings.Contains(failed, "join it again with the same link and name") {
		t.Fatalf("a chat outside a room should be told how to get back: %q", failed)
	}
	joined := restarted.must("join_room", map[string]any{"link": link, "name": "backend"})
	if !strings.Contains(joined["note"].(string), "under this name before") || joined["unread"] != float64(1) {
		t.Fatalf("%v", joined)
	}
	got := messages(restarted.must("read_messages", nil))
	if len(got) != 1 || got[0]["text"] != "are you there?" {
		t.Fatalf("lost what was waiting: %v", got)
	}
	members := app.must("list_members", nil)["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("restart created a second member: %v", members)
	}
}

func TestWaitingForAnAnswer(t *testing.T) {
	h := start(t)
	asker := h.chat()
	link := asker.must("create_room", map[string]any{"name": "app"})["room"].(string)
	answerer := h.chat()
	answerer.must("join_room", map[string]any{"link": link, "name": "backend"})

	var wg sync.WaitGroup
	wg.Add(1)
	var got []map[string]any
	began := time.Now()
	go func() {
		defer wg.Done()
		got = messages(asker.must("read_messages", map[string]any{"wait_seconds": 10}))
	}()
	time.Sleep(200 * time.Millisecond)
	answerer.must("send_message", map[string]any{"to": "app", "message": "here"})
	wg.Wait()
	if len(got) != 1 || time.Since(began) > 5*time.Second {
		t.Fatalf("waiting read did not wake up: %v after %v", got, time.Since(began))
	}
	out := asker.must("read_messages", map[string]any{"wait_seconds": 1})
	if len(messages(out)) != 0 || out["note"] != "nothing new yet" {
		t.Fatalf("%v", out)
	}
}

func TestMistakesGetClearAnswers(t *testing.T) {
	h := start(t)
	c := h.chat()
	for _, tool := range []string{"send_message", "list_members", "get_history"} {
		if _, failed := c.call(tool, map[string]any{"message": "x"}); !strings.Contains(failed, "not in a room") {
			t.Fatalf("%s: %q", tool, failed)
		}
	}
	if _, failed := c.call("join_room", map[string]any{"link": "hello", "name": "x"}); !strings.Contains(failed, "does not look like a room link") {
		t.Fatal(failed)
	}
	if _, failed := c.call("join_room", map[string]any{"link": h.url + "/r/AAAAAAAAAAAAAAAAAAAAAA", "name": "x"}); !strings.Contains(failed, "no such room") {
		t.Fatal(failed)
	}
	if _, failed := c.call("create_room", map[string]any{"name": "!!"}); !strings.Contains(failed, "give a name") {
		t.Fatal(failed)
	}
	c.must("create_room", map[string]any{"name": "app"})
	if _, failed := c.call("send_message", map[string]any{"to": "nobody", "message": "x"}); !strings.Contains(failed, `nobody named "nobody"`) {
		t.Fatal(failed)
	}
	if _, failed := c.call("send_message", map[string]any{"message": "   "}); !strings.Contains(failed, "empty") {
		t.Fatal(failed)
	}
	if _, failed := c.call("send_message", map[string]any{"message": strings.Repeat("x", maxMessageBytes+1)}); !strings.Contains(failed, "limit") {
		t.Fatal(failed)
	}
	if _, failed := c.call("send_message", map[string]any{"to": "app", "message": "x"}); failed != "that is you" {
		t.Fatal(failed)
	}
	_, out := c.rpc("tools/call", map[string]any{"name": "nope"})
	if out["error"] == nil {
		t.Fatalf("unknown tool: %v", out)
	}
}

func TestRateLimit(t *testing.T) {
	h := start(t)
	c := h.chat()
	c.must("create_room", map[string]any{"name": "app"})
	for range messagesPerMinute {
		c.must("send_message", map[string]any{"message": "x"})
	}
	if _, failed := c.call("send_message", map[string]any{"message": "x"}); !strings.Contains(failed, "wait a bit") {
		t.Fatal(failed)
	}
}

func getJSON(t *testing.T, url string, into any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil {
		json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode
}

func do(t *testing.T, method, url string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestRoomPageShowsWhoIsHereAndRemovesAndDeletes(t *testing.T) {
	h := start(t)
	resp, err := http.Post(h.url+"/api/rooms", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]string
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	token := created["token"]
	if resp.StatusCode != http.StatusCreated || len(token) != 22 || created["link"] != h.url+"/r/"+token {
		t.Fatalf("%d %v", resp.StatusCode, created)
	}
	page, err := http.Get(h.url + "/r/" + token)
	if err != nil {
		t.Fatal(err)
	}
	page.Body.Close()
	if page.StatusCode != http.StatusOK || page.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("room page: %d %v", page.StatusCode, page.Header)
	}

	a, b := h.chat(), h.chat()
	a.must("join_room", map[string]any{"link": created["link"], "name": "app"})
	b.must("join_room", map[string]any{"link": token, "name": "backend"})
	a.must("send_message", map[string]any{"to": "backend", "message": "first"})
	a.must("send_message", map[string]any{"message": "second"})

	var room struct {
		Members []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"members"`
		Messages []store.Message `json:"messages"`
		Invite   string          `json:"invite"`
	}
	if getJSON(t, h.url+"/api/rooms/"+token, &room) != http.StatusOK {
		t.Fatal("room state")
	}
	if len(room.Members) != 2 || room.Members[0].State != "online" || len(room.Messages) != 2 || !strings.Contains(room.Invite, token) {
		t.Fatalf("%+v", room)
	}
	first := room.Messages[0].ID
	room.Messages = nil
	getJSON(t, h.url+"/api/rooms/"+token+"?after="+strconv.FormatInt(first, 10), &room)
	if len(room.Messages) != 1 || room.Messages[0].Body != "second" {
		t.Fatalf("after: %+v", room.Messages)
	}

	if do(t, http.MethodDelete, h.url+"/api/rooms/"+token+"/members/backend") != http.StatusNoContent {
		t.Fatal("remove")
	}
	if _, failed := b.call("read_messages", nil); !strings.Contains(failed, "not in a room") {
		t.Fatalf("removed chat can still read: %q", failed)
	}
	if do(t, http.MethodDelete, h.url+"/api/rooms/"+token) != http.StatusNoContent {
		t.Fatal("delete")
	}
	if getJSON(t, h.url+"/api/rooms/"+token, nil) != http.StatusNotFound {
		t.Fatal("deleted room still answers")
	}
	if _, failed := a.call("send_message", map[string]any{"message": "x"}); !strings.Contains(failed, "not in a room") {
		t.Fatal(failed)
	}
}

func TestUnknownRoomsAndMalformedTokens(t *testing.T) {
	h := start(t)
	for _, path := range []string{"/api/rooms/AAAAAAAAAAAAAAAAAAAAAA", "/api/rooms/x", "/api/rooms/../../etc"} {
		if code := getJSON(t, h.url+path, nil); code != http.StatusNotFound {
			t.Fatalf("%s: %d", path, code)
		}
	}
}

func TestRoomCreationIsLimitedPerAddress(t *testing.T) {
	h := start(t)
	for range roomsPerHour {
		resp, _ := http.Post(h.url+"/api/rooms", "", nil)
		resp.Body.Close()
	}
	resp, _ := http.Post(h.url+"/api/rooms", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatal(resp.StatusCode)
	}
	c := h.chat()
	if _, failed := c.call("create_room", map[string]any{"name": "x"}); !strings.Contains(failed, "too many new rooms") {
		t.Fatal(failed)
	}
}

func TestTransportBasics(t *testing.T) {
	h := start(t)
	if code := getJSON(t, h.url+"/mcp", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp: %d", code)
	}
	c := h.chat()
	_, out := c.rpc("tools/list", nil)
	if n := len(out["result"].(map[string]any)["tools"].([]any)); n != 6 {
		t.Fatalf("%d tools", n)
	}
	req, _ := http.NewRequest(http.MethodPost, h.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatal(resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodDelete, h.url+"/mcp", nil)
	req.Header.Set("Mcp-Session-Id", c.session)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal(resp.StatusCode)
	}
	if code := getJSON(t, h.url+"/healthz", nil); code != http.StatusOK {
		t.Fatal(code)
	}
}

func TestShutdownEndsWaitingCalls(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "rooms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s := New(Config{Addr: "127.0.0.1:0"}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	ts.Start()
	defer ts.Close()
	c := &chat{t: t, url: ts.URL}
	resp, _ := c.rpc("initialize", map[string]any{})
	c.session = resp.Header.Get("Mcp-Session-Id")
	c.must("create_room", map[string]any{"name": "app"})
	done := make(chan int)
	go func() {
		resp, _ := c.rpc("tools/call", map[string]any{"name": "read_messages", "arguments": map[string]any{"wait_seconds": 60}})
		done <- resp.StatusCode
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("a call cut short by shutdown should say try again, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting call held up the shutdown")
	}
}
