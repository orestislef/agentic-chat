package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/orestislef/agentic-chat/internal/hub"
	"github.com/orestislef/agentic-chat/internal/store"
)

const testRedirect = "http://127.0.0.1:43210/callback"

type chatClient struct {
	t       *testing.T
	url     string
	token   string
	session string
}

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func postJSON(t *testing.T, target string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(target, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func registerClient(t *testing.T, ts *testServer) string {
	t.Helper()
	status, out := postJSON(t, ts.url+"/oauth/register", map[string]any{"client_name": "Test client", "redirect_uris": []string{testRedirect}})
	if status != http.StatusCreated {
		t.Fatalf("register client: %d %v", status, out)
	}
	return out["client_id"].(string)
}

// approve does what the consent screen does and returns the code from the redirect.
func approve(t *testing.T, u *client, clientID, workspace, verifier string) string {
	t.Helper()
	wsID, _ := strconv.ParseInt(workspace, 10, 64)
	var out struct {
		Redirect string `json:"redirect"`
	}
	u.mustJSON(http.MethodPost, "/api/oauth/approve", map[string]any{
		"client_id": clientID, "redirect_uri": testRedirect, "state": "xyz",
		"code_challenge": challengeFor(verifier), "code_challenge_method": "S256", "workspace_id": wsID,
	}, http.StatusOK, &out)
	target, err := url.Parse(out.Redirect)
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "127.0.0.1:43210" || target.Query().Get("state") != "xyz" || target.Query().Get("iss") == "" {
		t.Fatalf("redirect: %s", out.Redirect)
	}
	return target.Query().Get("code")
}

func exchange(t *testing.T, ts *testServer, clientID, code, verifier, redirect string) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(ts.url+"/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"code_verifier": {verifier}, "redirect_uri": {redirect},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// signIn runs the whole browser flow and returns a connection token.
func signIn(t *testing.T, ts *testServer, u *client, workspace string) string {
	t.Helper()
	clientID := registerClient(t, ts)
	code := approve(t, u, clientID, workspace, "a-long-enough-verifier")
	status, out := exchange(t, ts, clientID, code, "a-long-enough-verifier", testRedirect)
	if status != http.StatusOK {
		t.Fatalf("token: %d %v", status, out)
	}
	token := out["access_token"].(string)
	if !strings.HasPrefix(token, "acc_") {
		t.Fatalf("token %q", token)
	}
	return token
}

func (c *chatClient) rpc(method string, params any) (*http.Response, map[string]any) {
	c.t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, c.url+"/mcp", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(data, &out)
	return resp, out
}

func openChat(t *testing.T, ts *testServer, token string) *chatClient {
	t.Helper()
	c := &chatClient{t: t, url: ts.url, token: token}
	resp, out := c.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: %d", resp.StatusCode)
	}
	c.session = resp.Header.Get("Mcp-Session-Id")
	if c.session == "" {
		t.Fatal("no session id, chats could not be told apart")
	}
	if !strings.Contains(out["result"].(map[string]any)["instructions"].(string), "call join") {
		t.Fatal("instructions do not tell the chat to join")
	}
	return c
}

// tool returns the decoded result, or the error text in toolError.
func (c *chatClient) tool(name string, args map[string]any) map[string]any {
	c.t.Helper()
	resp, out := c.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("%s: status %d", name, resp.StatusCode)
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		c.t.Fatalf("%s: %v", name, out)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if result["isError"] == true {
		return map[string]any{"toolError": text}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		c.t.Fatalf("%s: %s", name, text)
	}
	return decoded
}

func TestDiscoveryPointsClientsToSignIn(t *testing.T) {
	ts := start(t)
	resp, err := http.Post(ts.url+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	challenge := resp.Header.Get("WWW-Authenticate")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(challenge, "resource_metadata=") {
		t.Fatalf("%d %q", resp.StatusCode, challenge)
	}
	meta := challenge[strings.Index(challenge, `"`)+1 : strings.LastIndex(challenge, `"`)]
	resp, err = http.Get(meta)
	if err != nil {
		t.Fatal(err)
	}
	var resource struct {
		Resource string   `json:"resource"`
		Servers  []string `json:"authorization_servers"`
	}
	json.NewDecoder(resp.Body).Decode(&resource)
	resp.Body.Close()
	if resource.Resource != ts.url+"/mcp" || len(resource.Servers) != 1 {
		t.Fatalf("%+v", resource)
	}
	resp, err = http.Get(resource.Servers[0] + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	var server map[string]any
	json.NewDecoder(resp.Body).Decode(&server)
	resp.Body.Close()
	if server["issuer"] != ts.url || server["registration_endpoint"] != ts.url+"/oauth/register" {
		t.Fatalf("%v", server)
	}
	methods := server["code_challenge_methods_supported"].([]any)
	if len(methods) != 1 || methods[0] != "S256" {
		t.Fatalf("pkce: %v", methods)
	}
}

func TestOnlyLocalRedirectsCanRegister(t *testing.T) {
	ts := start(t)
	bad := []string{
		"https://evil.example/callback", "http://evil.example/callback", "javascript:alert(1)",
		"http://localhost.evil.example/cb", "http://127.0.0.1.evil.example/cb", "http://user@evil.example@localhost/cb",
		"custom://callback", "",
	}
	for _, uri := range bad {
		status, _ := postJSON(t, ts.url+"/oauth/register", map[string]any{"redirect_uris": []string{uri}})
		if status != http.StatusBadRequest {
			t.Errorf("%q: status %d", uri, status)
		}
	}
	for _, uri := range []string{"http://localhost:1234/callback", "http://127.0.0.1:9/cb", "http://[::1]:80/cb"} {
		if status, out := postJSON(t, ts.url+"/oauth/register", map[string]any{"redirect_uris": []string{uri}}); status != http.StatusCreated {
			t.Errorf("%q: %d %v", uri, status, out)
		}
	}
}

func TestAuthorizePageRefusesBadClientWithoutRedirecting(t *testing.T) {
	ts := start(t)
	clientID := registerClient(t, ts)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cases := []struct {
		query string
		want  int
	}{
		{"client_id=nope&redirect_uri=" + url.QueryEscape(testRedirect), http.StatusBadRequest},
		{"client_id=" + clientID + "&redirect_uri=" + url.QueryEscape("https://evil.example/cb"), http.StatusBadRequest},
		{"client_id=" + clientID + "&redirect_uri=" + url.QueryEscape("http://127.0.0.1:1/other-path"), http.StatusBadRequest},
		{"client_id=" + clientID + "&redirect_uri=" + url.QueryEscape("http://127.0.0.1:55555/callback"), http.StatusOK}, // another free port is fine
	}
	for _, c := range cases {
		resp, err := noFollow.Get(ts.url + "/oauth/authorize?" + c.query)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d", c.query, resp.StatusCode)
		}
		if c.want == http.StatusOK && !bytes.Contains(body, []byte("/app.js")) {
			t.Errorf("did not serve the sign in page")
		}
	}
}

func TestApproveRequiresLoginMembershipAndPKCE(t *testing.T) {
	ts := start(t, func(c *Config) { c.Registration = RegistrationOpen })
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	wsID, _ := strconv.ParseInt(ws, 10, 64)
	stranger := ts.register("stranger", "")
	clientID := registerClient(t, ts)
	good := map[string]any{"client_id": clientID, "redirect_uri": testRedirect, "code_challenge": "abc", "code_challenge_method": "S256", "workspace_id": wsID}

	if status, _ := ts.newClient().do(http.MethodPost, "/api/oauth/approve", good); status != http.StatusUnauthorized {
		t.Fatalf("logged out: %d", status)
	}
	if status, _ := stranger.do(http.MethodPost, "/api/oauth/approve", good); status != http.StatusNotFound {
		t.Fatalf("someone else's workspace: %d", status)
	}
	for field, value := range map[string]any{"code_challenge": "", "code_challenge_method": "plain", "redirect_uri": "https://evil.example/cb"} {
		bad := map[string]any{}
		for k, v := range good {
			bad[k] = v
		}
		bad[field] = value
		if status, _ := owner.do(http.MethodPost, "/api/oauth/approve", bad); status != http.StatusBadRequest {
			t.Errorf("%s=%v: status %d", field, value, status)
		}
	}
}

func TestCodeExchangeChecksEverythingAndWorksOnce(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	clientID := registerClient(t, ts)
	verifier := "a-long-enough-verifier"

	wrong := []struct {
		name                     string
		client, verifier, target string
	}{
		{"wrong verifier", clientID, "not-the-verifier", testRedirect},
		{"wrong client", "acl_other", verifier, testRedirect},
		{"wrong redirect", clientID, verifier, "http://127.0.0.1:1/other"},
	}
	for _, c := range wrong {
		code := approve(t, owner, clientID, ws, verifier)
		status, out := exchange(t, ts, c.client, code, c.verifier, c.target)
		if status != http.StatusBadRequest || out["error"] != "invalid_grant" {
			t.Errorf("%s: %d %v", c.name, status, out)
		}
		// A failed attempt burns the code, so it cannot be guessed at.
		if status, _ := exchange(t, ts, clientID, code, verifier, testRedirect); status != http.StatusBadRequest {
			t.Errorf("%s: code still works after a failed attempt", c.name)
		}
	}

	code := approve(t, owner, clientID, ws, verifier)
	if status, out := exchange(t, ts, clientID, code, verifier, testRedirect); status != http.StatusOK {
		t.Fatalf("good exchange: %d %v", status, out)
	}
	if status, _ := exchange(t, ts, clientID, code, verifier, testRedirect); status != http.StatusBadRequest {
		t.Fatal("code worked twice")
	}
}

func TestEachChatIsItsOwnAgent(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	token := signIn(t, ts, owner, ws)

	backend := openChat(t, ts, token)
	frontend := openChat(t, ts, token)
	if backend.session == frontend.session {
		t.Fatal("two chats share a session")
	}
	if got := backend.tool("join", map[string]any{"name": "Backend"}); got["you"] != "backend" {
		t.Fatalf("join: %v", got)
	}
	if got := frontend.tool("join", map[string]any{"name": "frontend"}); got["you"] != "frontend" {
		t.Fatalf("join: %v", got)
	}

	// A second chat in the same project gets its own name, and is told.
	second := openChat(t, ts, token)
	got := second.tool("join", map[string]any{"name": "backend"})
	if got["you"] != "backend-2" || !strings.Contains(got["note"].(string), "in use") {
		t.Fatalf("second chat: %v", got)
	}

	// They talk to each other, and the sender's name comes from the session.
	sent := frontend.tool("ask_agent", map[string]any{"to": "backend", "message": "does /users paginate", "wait_seconds": 0})
	if sent["message_id"] == nil {
		t.Fatalf("ask: %v", sent)
	}
	// Any tool result tells a working chat that something is waiting.
	agents := backend.tool("list_agents", nil)
	if agents["unread_messages"] != float64(1) {
		t.Fatalf("no hint about the waiting message: %v", agents)
	}
	inbox := backend.tool("check_inbox", nil)
	messages := inbox["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["from"] != "frontend" {
		t.Fatalf("inbox: %v", inbox)
	}
	if again := backend.tool("list_agents", nil); again["unread_messages"] != nil {
		t.Fatalf("hint after reading: %v", again)
	}
	backend.tool("reply", map[string]any{"message_id": sent["message_id"], "message": "yes, cursors"})
	answer := frontend.tool("wait_for_reply", map[string]any{"message_id": sent["message_id"], "wait_seconds": 1})
	if answer["status"] != "answered" {
		t.Fatalf("answer: %v", answer)
	}

	// The web UI sees them all.
	var list []agentView
	owner.mustJSON(http.MethodGet, "/api/workspaces/"+ws+"/agents", nil, http.StatusOK, &list)
	for _, want := range []string{"backend", "backend-2", "frontend"} {
		if stateOf(list, want) == "" {
			t.Errorf("%s missing from the web UI", want)
		}
	}
}

func TestReopeningAProjectKeepsNameAndInbox(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	token := signIn(t, ts, owner, owner.workspaceID())

	first := openChat(t, ts, token)
	first.tool("join", map[string]any{"name": "backend"})
	other := openChat(t, ts, token)
	other.tool("join", map[string]any{"name": "frontend"})

	// The chat closes, which clients announce with a DELETE.
	req, _ := http.NewRequest(http.MethodDelete, ts.url+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", first.session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	other.tool("send_message", map[string]any{"to": "backend", "message": "while you were away"})

	reopened := openChat(t, ts, token)
	got := reopened.tool("join", map[string]any{"name": "backend"})
	if got["you"] != "backend" || got["new"] != false {
		t.Fatalf("reopen: %v", got)
	}
	inbox := reopened.tool("check_inbox", nil)
	if len(inbox["messages"].([]any)) != 1 {
		t.Fatalf("inbox lost: %v", inbox)
	}
}

func TestChatThatSkipsJoinStillWorks(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	token := signIn(t, ts, owner, owner.workspaceID())
	chat := openChat(t, ts, token)
	if got := chat.tool("list_agents", nil); got["you"] != "chat" {
		t.Fatalf("%v", got)
	}
	if got := chat.tool("join", map[string]any{"name": "!!!"}); got["toolError"] == nil {
		t.Fatalf("empty name accepted: %v", got)
	}
	if got := chat.tool("join", map[string]any{"name": "real"}); got["you"] != "real" {
		t.Fatalf("%v", got)
	}
	if got := chat.tool("list_agents", nil); got["you"] != "real" {
		t.Fatalf("name did not stick: %v", got)
	}
}

func TestHealthCheckCreatesNoAgent(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	token := signIn(t, ts, owner, ws)
	chat := openChat(t, ts, token)
	resp, out := chat.rpc("tools/list", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d", resp.StatusCode)
	}
	tools := out["result"].(map[string]any)["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "join" {
		t.Fatalf("join should come first: %v", tools[0])
	}
	var list []agentView
	owner.mustJSON(http.MethodGet, "/api/workspaces/"+ws+"/agents", nil, http.StatusOK, &list)
	if len(list) != 1 { // only the person
		t.Fatalf("connecting created agents: %+v", list)
	}
}

func TestRemovedChatAgentIsToldAndCanRejoin(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	token := signIn(t, ts, owner, ws)
	chat := openChat(t, ts, token)
	chat.tool("join", map[string]any{"name": "backend"})

	var list []agentView
	owner.mustJSON(http.MethodGet, "/api/workspaces/"+ws+"/agents", nil, http.StatusOK, &list)
	for _, a := range list {
		if a.Name == "backend" {
			owner.mustJSON(http.MethodDelete, "/api/workspaces/"+ws+"/agents/"+strconv.FormatInt(a.ID, 10), nil, http.StatusNoContent, nil)
		}
	}
	got := chat.tool("list_agents", nil)
	if got["toolError"] == nil || !strings.Contains(got["toolError"].(string), "Tell your user") {
		t.Fatalf("removed chat was not told: %v", got)
	}
	if got := chat.tool("join", map[string]any{"name": "backend"}); got["you"] != "backend" || got["new"] != true {
		t.Fatalf("rejoin: %v", got)
	}
}

func TestConnectionsAreSeparatedAndCanBeSignedOut(t *testing.T) {
	ts := start(t, func(c *Config) { c.Registration = RegistrationOpen })
	alice := ts.register("alice", "")
	bob := ts.register("bob", "")
	aliceToken := signIn(t, ts, alice, alice.workspaceID())
	bobToken := signIn(t, ts, bob, bob.workspaceID())

	a := openChat(t, ts, aliceToken)
	a.tool("join", map[string]any{"name": "secretbot"})
	a.tool("send_message", map[string]any{"to": "alice", "message": "private plans"})

	// Bob's chat sees nothing of Alice's workspace, and can use the same names.
	b := openChat(t, ts, bobToken)
	if got := b.tool("join", map[string]any{"name": "secretbot"}); got["you"] != "secretbot" {
		t.Fatalf("%v", got)
	}
	if got := b.tool("search_history", map[string]any{"query": "private"}); len(got["messages"].([]any)) != 0 {
		t.Fatalf("search crossed workspaces: %v", got)
	}
	if got := b.tool("send_message", map[string]any{"to": "alice", "message": "hi"}); got["toolError"] == nil {
		t.Fatalf("reached another workspace: %v", got)
	}

	// Someone else's session id does not help either.
	stolen := &chatClient{t: t, url: ts.url, token: bobToken, session: a.session}
	if got := stolen.tool("list_agents", nil); got["you"] == "secretbot" && got["agents"] != nil {
		for _, entry := range got["agents"].([]any) {
			if entry.(map[string]any)["name"] == "alice" {
				t.Fatal("a session id gave access across connections")
			}
		}
	}

	var list []store.Connection
	alice.mustJSON(http.MethodGet, "/api/me/connections", nil, http.StatusOK, &list)
	if len(list) != 1 || list[0].ClientName != "Test client" {
		t.Fatalf("connections: %+v", list)
	}
	id := strconv.FormatInt(list[0].ID, 10)
	if status, _ := bob.do(http.MethodDelete, "/api/me/connections/"+id, nil); status != http.StatusNotFound {
		t.Fatalf("signed out someone else: %d", status)
	}
	alice.mustJSON(http.MethodDelete, "/api/me/connections/"+id, nil, http.StatusNoContent, nil)
	resp, _ := a.rpc("tools/list", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("still works after signing out: %d", resp.StatusCode)
	}
}

func TestRemovedMemberLosesConnection(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	var created struct {
		Code string `json:"code"`
	}
	owner.mustJSON(http.MethodPost, "/api/workspaces/"+ws+"/invites", map[string]int{"max_uses": 1}, http.StatusCreated, &created)
	guest := ts.register("guest", created.Code)
	token := signIn(t, ts, guest, ws)
	chat := openChat(t, ts, token)
	chat.tool("join", map[string]any{"name": "guestbot"})

	var members []store.Member
	owner.mustJSON(http.MethodGet, "/api/workspaces/"+ws+"/members", nil, http.StatusOK, &members)
	for _, m := range members {
		if m.Username == "guest" {
			owner.mustJSON(http.MethodDelete, "/api/workspaces/"+ws+"/members/"+strconv.FormatInt(m.UserID, 10), nil, http.StatusNoContent, nil)
		}
	}
	resp, _ := chat.rpc("tools/list", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("removed member's client still connected: %d", resp.StatusCode)
	}
}

func TestDemoCapsChatAgents(t *testing.T) {
	ts := start(t, func(c *Config) { c.Registration = RegistrationOpen; c.Demo = true })
	ts.register("admin", "")
	visitor := ts.register("visitor", "")
	token := signIn(t, ts, visitor, visitor.workspaceID())
	for _, name := range []string{"a", "b", "c"} {
		if got := openChat(t, ts, token).tool("join", map[string]any{"name": name}); got["you"] != name {
			t.Fatalf("%v", got)
		}
	}
	got := openChat(t, ts, token).tool("join", map[string]any{"name": "d"})
	if got["toolError"] == nil || !strings.Contains(got["toolError"].(string), "limit") {
		t.Fatalf("over the demo limit: %v", got)
	}
}

func TestAgentTokensStillWorkAsBefore(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	_, token := owner.createAgent(owner.workspaceID(), "manual")
	c := &chatClient{t: t, url: ts.url, token: token}
	resp, out := c.rpc("initialize", map[string]any{})
	if resp.Header.Get("Mcp-Session-Id") != "" {
		t.Fatal("an agent token does not need a session")
	}
	if strings.Contains(out["result"].(map[string]any)["instructions"].(string), "call join") {
		t.Fatal("an agent with a token should not be told to join")
	}
	_, list := c.rpc("tools/list", nil)
	for _, tool := range list["result"].(map[string]any)["tools"].([]any) {
		if tool.(map[string]any)["name"] == "join" {
			t.Fatal("join offered to an agent with its own token")
		}
	}
	if got := c.tool("list_agents", nil); got["you"] != "manual" {
		t.Fatalf("%v", got)
	}
}

func TestReopenedProjectSettlesOnItsNameAndGetsItsMessages(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "settle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := Config{DataDir: dir, DownloadsDir: dir, Registration: RegistrationInvite, Limits: hub.DefaultLimits()}
	srv := httptest.NewServer(New(cfg, s, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	defer srv.Close()
	ts := &testServer{t: t, url: srv.URL}
	owner := ts.register("owner", "")
	token := signIn(t, ts, owner, owner.workspaceID())

	// The old chat closed without saying so, so its name still looks busy.
	closed := openChat(t, ts, token)
	closed.tool("join", map[string]any{"name": "backend"})
	other := openChat(t, ts, token)
	other.tool("join", map[string]any{"name": "frontend"})
	other.tool("send_message", map[string]any{"to": "backend", "message": "for the old name"})

	reopened := openChat(t, ts, token)
	if got := reopened.tool("join", map[string]any{"name": "backend"}); got["you"] != "backend-2" {
		t.Fatalf("%v", got)
	}
	other.tool("send_message", map[string]any{"to": "backend-2", "message": "for the stand-in"})

	// Nothing changes while the old chat might still be alive.
	if got := reopened.tool("list_agents", nil); got["you"] != "backend-2" || got["name_changed"] != nil {
		t.Fatalf("settled too early: %v", got)
	}

	// Later the old chat counts as gone, and the next call settles things by itself.
	endSession(t, ts, token, closed.session)
	got := reopened.tool("list_agents", nil)
	if got["you"] != "backend" || got["name_changed"] == nil {
		t.Fatalf("did not settle: %v", got)
	}
	if got["unread_messages"] != float64(2) {
		t.Fatalf("waiting messages not handed over: %v", got)
	}
	inbox := reopened.tool("check_inbox", nil)
	if len(inbox["messages"].([]any)) != 2 {
		t.Fatalf("inbox: %v", inbox)
	}
	for _, entry := range other.tool("list_agents", nil)["agents"].([]any) {
		if entry.(map[string]any)["name"] == "backend-2" {
			t.Fatal("stand-in name still listed")
		}
	}
}

func TestFolderThatReallyHasASuffixKeepsIt(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	token := signIn(t, ts, owner, owner.workspaceID())
	chat := openChat(t, ts, token)
	chat.tool("join", map[string]any{"name": "app-2"})
	if got := chat.tool("list_agents", nil); got["you"] != "app-2" || got["name_changed"] != nil {
		t.Fatalf("%v", got)
	}
}

func endSession(t *testing.T, ts *testServer, token, session string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, ts.url+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
