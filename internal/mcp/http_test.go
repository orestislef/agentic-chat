package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orestislef/agentic-chat/internal/auth"
	"github.com/orestislef/agentic-chat/internal/hub"
	"github.com/orestislef/agentic-chat/internal/store"
)

type fixture struct {
	url                   string
	frontToken, backToken string
	revokeBackend         func(*testing.T)
}

func setup(t *testing.T) *fixture {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	u, err := s.CreateAccount(ctx, "owner", "h", "", "team")
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.WorkspacesForUser(ctx, u.ID)
	f := &fixture{frontToken: "ac_front", backToken: "ac_back"}
	s.CreateAgent(ctx, list[0].ID, u.ID, "frontend", "web ui", auth.HashToken(f.frontToken))
	back, _ := s.CreateAgent(ctx, list[0].ID, u.ID, "backend", "api", auth.HashToken(f.backToken))
	f.revokeBackend = func(t *testing.T) {
		t.Helper()
		if err := s.RevokeAgent(ctx, list[0].ID, back.ID); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHandler(s, hub.New(s, hub.DefaultLimits(), log), log))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fixture) rpc(t *testing.T, token, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, f.url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d", method, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// call runs a tool and decodes its JSON text, failing on a tool error.
func (f *fixture) call(t *testing.T, token, tool string, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := f.callRaw(t, token, tool, args)
	if isErr {
		t.Fatalf("%s failed: %s", tool, text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s returned non JSON: %s", tool, text)
	}
	return out
}

func (f *fixture) callRaw(t *testing.T, token, tool string, args map[string]any) (string, bool) {
	t.Helper()
	resp := f.rpc(t, token, "tools/call", map[string]any{"name": tool, "arguments": args})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no result: %v", tool, resp)
	}
	isErr, _ := result["isError"].(bool)
	return result["content"].([]any)[0].(map[string]any)["text"].(string), isErr
}

func TestRejectsBadTokens(t *testing.T) {
	f := setup(t)
	for _, header := range []string{"", "Bearer nope", "Basic abc"} {
		req, _ := http.NewRequest(http.MethodPost, f.url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("header %q: status %d", header, resp.StatusCode)
		}
	}
}

func TestInitializeNegotiation(t *testing.T) {
	f := setup(t)
	cases := map[string]string{
		"2025-06-18": "2025-06-18",
		"2026-07-28": supportedVersions[0], // must fall back, that revision cannot push
		"1999-01-01": supportedVersions[0],
	}
	for asked, want := range cases {
		resp := f.rpc(t, f.frontToken, "initialize", map[string]any{"protocolVersion": asked})
		got := resp["result"].(map[string]any)["protocolVersion"]
		if got != want {
			t.Errorf("asked %s: got %v, want %s", asked, got, want)
		}
	}
}

func TestToolsListIsValid(t *testing.T) {
	f := setup(t)
	resp := f.rpc(t, f.frontToken, "tools/list", nil)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(Tools()) {
		t.Fatalf("got %d tools", len(tools))
	}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if tool["description"] == "" {
			t.Errorf("%v has no description", tool["name"])
		}
		if tool["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("%v has a bad schema", tool["name"])
		}
	}
}

func TestConversationOverTools(t *testing.T) {
	f := setup(t)

	list := f.call(t, f.frontToken, "list_agents", nil)
	if list["you"] != "frontend" {
		t.Fatalf("you = %v", list["you"])
	}

	// Backend has not used the hub yet, so the question is queued.
	asked := f.call(t, f.frontToken, "ask_agent", map[string]any{"to": "backend", "message": "does /users paginate"})
	if asked["status"] != "queued" {
		t.Fatalf("expected queued, got %v", asked)
	}
	messageID := asked["message_id"].(float64)

	inbox := f.call(t, f.backToken, "check_inbox", nil)
	messages := inbox["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["from"] != "frontend" {
		t.Fatalf("inbox: %v", inbox)
	}
	if again := f.call(t, f.backToken, "check_inbox", nil); len(again["messages"].([]any)) != 0 {
		t.Fatal("inbox should be empty once read")
	}

	f.call(t, f.backToken, "reply", map[string]any{"message_id": messageID, "message": "yes, cursor based"})

	answered := f.call(t, f.frontToken, "wait_for_reply", map[string]any{"message_id": messageID, "wait_seconds": 1})
	if answered["status"] != "answered" {
		t.Fatalf("got %v", answered)
	}
	if body := answered["reply"].(map[string]any)["body"]; body != "yes, cursor based" {
		t.Fatalf("body %v", body)
	}

	history := f.call(t, f.backToken, "get_history", map[string]any{"with": "frontend"})
	if len(history["messages"].([]any)) != 2 {
		t.Fatalf("history: %v", history)
	}
	hits := f.call(t, f.backToken, "search_history", map[string]any{"query": "cursor"})
	if len(hits["messages"].([]any)) != 1 {
		t.Fatalf("search: %v", hits)
	}
}

func TestAskOnlineAgentTimesOutAsPending(t *testing.T) {
	f := setup(t)
	f.call(t, f.backToken, "list_agents", nil) // marks backend as recently active
	asked := f.call(t, f.frontToken, "ask_agent", map[string]any{"to": "backend", "message": "q", "wait_seconds": 0})
	if asked["status"] != "pending" {
		t.Fatalf("got %v", asked)
	}
}

func TestToolErrorsAreReadable(t *testing.T) {
	f := setup(t)
	text, isErr := f.callRaw(t, f.frontToken, "ask_agent", map[string]any{"to": "nobody", "message": "hi"})
	if !isErr || !strings.Contains(text, "list_agents") {
		t.Fatalf("unhelpful error %q (isError=%v)", text, isErr)
	}
	resp := f.rpc(t, f.frontToken, "tools/call", map[string]any{"name": "no_such_tool"})
	if _, ok := resp["error"]; !ok {
		t.Fatalf("unknown tool should be a protocol error: %v", resp)
	}
}

func TestNotificationGetsNoBody(t *testing.T) {
	f := setup(t)
	req, _ := http.NewRequest(http.MethodPost, f.url, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+f.frontToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestGetIsNotAllowed(t *testing.T) {
	f := setup(t)
	resp, err := http.Get(f.url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatal(fmt.Sprint("status ", resp.StatusCode))
	}
}

func TestWaitingOnARemovedAgentSaysSo(t *testing.T) {
	f := setup(t)
	asked := f.call(t, f.frontToken, "ask_agent", map[string]any{"to": "backend", "message": "q"})
	f.revokeBackend(t)
	got := f.call(t, f.frontToken, "wait_for_reply", map[string]any{"message_id": asked["message_id"], "wait_seconds": 30})
	if got["status"] != "unreachable" || !strings.Contains(got["note"].(string), "tell your user") {
		t.Fatalf("got %v", got)
	}
	text, isErr := f.callRaw(t, f.frontToken, "ask_agent", map[string]any{"to": "backend", "message": "again"})
	if !isErr || !strings.Contains(text, "list_agents") {
		t.Fatalf("asking a removed agent: %q %v", text, isErr)
	}
}
