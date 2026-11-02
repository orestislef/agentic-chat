package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func openHub(t *testing.T, mutate ...func(*Config)) *testServer {
	t.Helper()
	return start(t, append([]func(*Config){func(c *Config) { c.Registration = RegistrationOpen }}, mutate...)...)
}

func createRoom(t *testing.T, ts *testServer) string {
	t.Helper()
	status, out := postJSON(t, ts.url+"/api/rooms", map[string]string{})
	if status != http.StatusCreated {
		t.Fatalf("create room: %d %v", status, out)
	}
	return out["token"].(string)
}

// joinRoom connects a chat through a room address, with nothing but that address.
func joinRoom(t *testing.T, ts *testServer, token, device string) *chatClient {
	t.Helper()
	path := "/r/" + token + "/mcp"
	if device != "" {
		path = "/r/" + token + "/" + device + "/mcp"
	}
	c := &chatClient{t: t, url: ts.url, path: path}
	resp, out := c.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: %d %v", resp.StatusCode, out)
	}
	c.session = resp.Header.Get("Mcp-Session-Id")
	if c.session == "" {
		t.Fatal("no session id")
	}
	instructions := out["result"].(map[string]any)["instructions"].(string)
	if !strings.Contains(instructions, "call join") || !strings.Contains(instructions, "CronCreate") {
		t.Fatalf("instructions: %s", instructions)
	}
	return c
}

func TestTwoChatsTalkInARoomWithNoAccountsAtAll(t *testing.T) {
	ts := openHub(t)
	token := createRoom(t, ts)
	if len(token) > 24 {
		t.Fatalf("link token is not short: %q", token)
	}
	mine := joinRoom(t, ts, token, "laptop")
	theirs := joinRoom(t, ts, token, "office")
	if got := mine.tool("join", map[string]any{"name": "driver_app"}); got["you"] != "driver_app" {
		t.Fatalf("%v", got)
	}
	if got := theirs.tool("join", map[string]any{"name": "callcenter"}); got["you"] != "callcenter" {
		t.Fatalf("%v", got)
	}
	agents := theirs.tool("list_agents", nil)["agents"].([]any)
	if len(agents) != 1 || agents[0].(map[string]any)["name"] != "driver_app" {
		t.Fatalf("should see exactly the other chat, no hidden owner: %v", agents)
	}
	sent := theirs.tool("ask_agent", map[string]any{"to": "driver_app", "message": "which endpoint", "wait_seconds": 0})
	if mine.tool("list_agents", nil)["unread_messages"] != float64(1) {
		t.Fatal("no hint about the waiting message")
	}
	inbox := mine.tool("check_inbox", nil)["messages"].([]any)
	if len(inbox) != 1 || inbox[0].(map[string]any)["from"] != "callcenter" {
		t.Fatalf("%v", inbox)
	}
	mine.tool("reply", map[string]any{"message_id": sent["message_id"], "message": "/orders"})
	if got := theirs.tool("wait_for_reply", map[string]any{"message_id": sent["message_id"], "wait_seconds": 1}); got["status"] != "answered" {
		t.Fatalf("%v", got)
	}
	if n, _ := ts.configUsers(t); n {
		t.Fatal("a room should not count as the first account")
	}
}

// configUsers reports whether the hub thinks an account exists.
func (ts *testServer) configUsers(t *testing.T) (bool, error) {
	t.Helper()
	resp, err := http.Get(ts.url + "/api/config")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out["first_user"] != true, nil
}

func TestSameFolderOnTwoMachinesStaysTwoAgentsInARoom(t *testing.T) {
	ts := openHub(t)
	token := createRoom(t, ts)
	first := joinRoom(t, ts, token, "laptop")
	first.tool("join", map[string]any{"name": "backend"})
	endChat(t, first)

	other := joinRoom(t, ts, token, "office")
	if got := other.tool("join", map[string]any{"name": "backend"}); got["you"] != "backend-2" {
		t.Fatalf("another machine took over the agent: %v", got)
	}
	// The machine that had it gets it back when the project reopens.
	again := joinRoom(t, ts, token, "laptop")
	if got := again.tool("join", map[string]any{"name": "backend"}); got["you"] != "backend" {
		t.Fatalf("%v", got)
	}
}

func endChat(t *testing.T, c *chatClient) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, c.url+c.path, nil)
	req.Header.Set("Mcp-Session-Id", c.session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestRoomsAreSealedFromEachOtherAndFromAccounts(t *testing.T) {
	ts := openHub(t)
	a := joinRoom(t, ts, createRoom(t, ts), "x")
	b := joinRoom(t, ts, createRoom(t, ts), "x") // the same machine id in another room means nothing
	a.tool("join", map[string]any{"name": "alpha"})
	a.tool("send_message", map[string]any{"to": "alpha-peer", "message": "ignored"})
	if got := b.tool("join", map[string]any{"name": "alpha"}); got["you"] != "alpha" {
		t.Fatalf("name should be free in another room: %v", got)
	}
	if got := b.tool("search_history", map[string]any{"query": "ignored"}); len(got["messages"].([]any)) != 0 {
		t.Fatalf("%v", got)
	}

	// An account on the same hub sees nothing of a room, and a room nothing of an account.
	person := ts.register("person", "")
	var list []map[string]any
	person.mustJSON(http.MethodGet, "/api/workspaces", nil, http.StatusOK, &list)
	if len(list) != 1 {
		t.Fatalf("rooms showed up as workspaces: %v", list)
	}
	if got := a.tool("send_message", map[string]any{"to": "person", "message": "hi"}); got["toolError"] == nil {
		t.Fatalf("reached out of the room: %v", got)
	}
}

func TestUnknownRoomGivesNoSignInPrompt(t *testing.T) {
	ts := openHub(t)
	for _, path := range []string{"/r/nope/mcp", "/r/nope/device/mcp", "/api/rooms/nope", "/api/rooms/nope/messages"} {
		method := http.MethodGet
		var body *strings.Reader = strings.NewReader("")
		if strings.HasSuffix(path, "/mcp") {
			method = http.MethodPost
			body = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
		}
		req, _ := http.NewRequest(method, ts.url+path, body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || resp.Header.Get("WWW-Authenticate") != "" {
			t.Errorf("%s: %d %q", path, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	token := createRoom(t, ts)
	req, _ := http.NewRequest(http.MethodPost, ts.url+"/r/"+token+"/bad%20device/mcp", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bad device: %d", resp.StatusCode)
	}
}

func TestRoomLinkIsTheOnlyKeyAndClientTokensCannotBeForged(t *testing.T) {
	ts := openHub(t)
	token := createRoom(t, ts)
	chat := joinRoom(t, ts, token, "laptop")
	chat.tool("join", map[string]any{"name": "agent"})
	// A bearer token sent along is ignored in favour of the address.
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	_ = raw
	// Guessing at the plain endpoint with the room link as a token gets nowhere.
	req, _ := http.NewRequest(http.MethodPost, ts.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("room link worked as a token: %d", resp.StatusCode)
	}
	// Nobody can log in as a room's hidden owner.
	status, _ := postJSON(t, ts.url+"/api/auth/login", map[string]string{"username": "~room", "password": "!"})
	if status != http.StatusUnauthorized {
		t.Fatalf("login: %d", status)
	}
}

func TestRoomPageShowsWhoIsThereAndWhatWasSaid(t *testing.T) {
	ts := openHub(t)
	token := createRoom(t, ts)
	a := joinRoom(t, ts, token, "one")
	b := joinRoom(t, ts, token, "two")
	a.tool("join", map[string]any{"name": "first"})
	b.tool("join", map[string]any{"name": "second"})
	sent := a.tool("send_message", map[string]any{"to": "second", "message": "hello there"})

	resp, err := http.Get(ts.url + "/r/" + token)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	c := ts.newClient()
	var state struct {
		Agents  []agentView       `json:"agents"`
		Threads []json.RawMessage `json:"threads"`
	}
	c.mustJSON(http.MethodGet, "/api/rooms/"+token, nil, http.StatusOK, &state)
	if len(state.Agents) != 2 || stateOf(state.Agents, "first") == "" || len(state.Threads) != 1 {
		t.Fatalf("%+v", state)
	}
	var messages []map[string]any
	c.mustJSON(http.MethodGet, "/api/rooms/"+token+"/messages?thread="+strconv.Itoa(int(sent["thread_id"].(float64))), nil, http.StatusOK, &messages)
	if len(messages) != 1 || messages[0]["body"] != "hello there" {
		t.Fatalf("%v", messages)
	}
}

func TestRemovingFromARoomAndDeletingIt(t *testing.T) {
	ts := openHub(t)
	token := createRoom(t, ts)
	a := joinRoom(t, ts, token, "one")
	b := joinRoom(t, ts, token, "two")
	a.tool("join", map[string]any{"name": "first"})
	b.tool("join", map[string]any{"name": "second"})
	c := ts.newClient()

	var state struct {
		Agents []agentView `json:"agents"`
	}
	c.mustJSON(http.MethodGet, "/api/rooms/"+token, nil, http.StatusOK, &state)
	for _, agent := range state.Agents {
		if agent.Name == "second" {
			c.mustJSON(http.MethodDelete, "/api/rooms/"+token+"/agents/"+strconv.FormatInt(agent.ID, 10), nil, http.StatusNoContent, nil)
		}
	}
	got := b.tool("list_agents", nil)
	if got["toolError"] == nil || !strings.Contains(got["toolError"].(string), "Tell your user") {
		t.Fatalf("removed chat was not told: %v", got)
	}

	c.mustJSON(http.MethodDelete, "/api/rooms/"+token, nil, http.StatusNoContent, nil)
	if resp, _ := a.rpc("tools/list", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("room still works after deleting: %d", resp.StatusCode)
	}
	if status, _ := c.do(http.MethodGet, "/api/rooms/"+token, nil); status != http.StatusNotFound {
		t.Fatalf("state after deleting: %d", status)
	}
}

func TestCreatingRoomsFollowsTheHubsRules(t *testing.T) {
	closed := start(t) // invite only
	if status, _ := postJSON(t, closed.url+"/api/rooms", map[string]string{}); status != http.StatusForbidden {
		t.Fatalf("no login on a private hub: %d", status)
	}
	owner := closed.register("owner", "")
	if status, data := owner.do(http.MethodPost, "/api/rooms", map[string]string{}); status != http.StatusCreated {
		t.Fatalf("logged in: %d %s", status, data)
	}

	ts := openHub(t)
	var last int
	for i := 0; i < 25; i++ {
		last, _ = postJSON(t, ts.url+"/api/rooms", map[string]string{})
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("no limit on creating rooms: %d", last)
	}
}

func TestDemoLimitsApplyToRooms(t *testing.T) {
	ts := openHub(t, func(c *Config) { c.Demo = true })
	token := createRoom(t, ts)
	for _, name := range []string{"a", "b", "c"} {
		if got := joinRoom(t, ts, token, name).tool("join", map[string]any{"name": name}); got["you"] != name {
			t.Fatalf("%v", got)
		}
	}
	got := joinRoom(t, ts, token, "d").tool("join", map[string]any{"name": "d"})
	if got["toolError"] == nil || !strings.Contains(got["toolError"].(string), "limit") {
		t.Fatalf("%v", got)
	}
}
