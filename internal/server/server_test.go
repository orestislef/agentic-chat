package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/orestislef/agentic-chat/internal/hub"
	"github.com/orestislef/agentic-chat/internal/store"
)

const password = "a long enough password"

type testServer struct {
	t   *testing.T
	url string
}

func start(t *testing.T, mutate ...func(*Config)) *testServer {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{DataDir: dir, DownloadsDir: dir, Registration: RegistrationInvite, Limits: hub.DefaultLimits()}
	for _, m := range mutate {
		m(&cfg)
	}
	s, err := store.Open(filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv := httptest.NewServer(New(cfg, s, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)
	return &testServer{t: t, url: srv.URL}
}

// client is a browser-like session with its own cookie jar.
type client struct {
	ts   *testServer
	http *http.Client
}

func (ts *testServer) newClient() *client {
	jar, _ := cookiejar.New(nil)
	return &client{ts: ts, http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any) (int, []byte) {
	c.ts.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, c.ts.url+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.ts.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (c *client) mustJSON(method, path string, body any, wantStatus int, into any) {
	c.ts.t.Helper()
	status, data := c.do(method, path, body)
	if status != wantStatus {
		c.ts.t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, wantStatus, data)
	}
	if into != nil {
		if err := json.Unmarshal(data, into); err != nil {
			c.ts.t.Fatalf("%s %s: %v: %s", method, path, err, data)
		}
	}
}

func (ts *testServer) register(username, invite string) *client {
	c := ts.newClient()
	c.mustJSON(http.MethodPost, "/api/auth/register", map[string]string{
		"username": username, "password": password, "invite": invite,
	}, http.StatusCreated, nil)
	return c
}

func (c *client) workspaceID() string {
	var list []store.Workspace
	c.mustJSON(http.MethodGet, "/api/workspaces", nil, http.StatusOK, &list)
	if len(list) == 0 {
		c.ts.t.Fatal("no workspaces")
	}
	return strconv.FormatInt(list[0].ID, 10)
}

func (c *client) createAgent(ws, name string) (int64, string) {
	var out struct {
		Agent agentView `json:"agent"`
		Token string    `json:"token"`
	}
	c.mustJSON(http.MethodPost, "/api/workspaces/"+ws+"/agents", map[string]string{"name": name}, http.StatusCreated, &out)
	return out.Agent.ID, out.Token
}

func TestRegistrationModes(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")

	// Invite mode: the second person without a code is turned away.
	status, _ := ts.newClient().do(http.MethodPost, "/api/auth/register", map[string]string{"username": "stranger", "password": password})
	if status != http.StatusForbidden {
		t.Fatalf("no invite: status %d", status)
	}

	var created struct {
		Code string `json:"code"`
	}
	ws := owner.workspaceID()
	owner.mustJSON(http.MethodPost, "/api/workspaces/"+ws+"/invites", map[string]int{"max_uses": 1}, http.StatusCreated, &created)
	if !strings.HasPrefix(created.Code, "aci_") {
		t.Fatalf("code %q", created.Code)
	}
	guest := ts.register("guest", created.Code)
	if guest.workspaceID() != ws {
		t.Fatal("invited user landed in the wrong workspace")
	}

	// Closed mode: nobody gets in after the first account.
	closed := start(t, func(c *Config) { c.Registration = RegistrationClosed })
	closed.register("first", "")
	status, _ = closed.newClient().do(http.MethodPost, "/api/auth/register", map[string]string{"username": "second", "password": password})
	if status != http.StatusForbidden {
		t.Fatalf("closed: status %d", status)
	}

	// Open mode: anyone gets a personal workspace.
	open := start(t, func(c *Config) { c.Registration = RegistrationOpen })
	open.register("first", "")
	a := open.register("second", "")
	if a.workspaceID() == "" {
		t.Fatal("open registration gave no workspace")
	}
}

func TestRegisterValidation(t *testing.T) {
	ts := start(t)
	c := ts.newClient()
	cases := []map[string]string{
		{"username": "x", "password": password},
		{"username": "Has Space", "password": password},
		{"username": "<script>", "password": password},
		{"username": "shortpass", "password": "short"},
	}
	for _, body := range cases {
		if status, _ := c.do(http.MethodPost, "/api/auth/register", body); status != http.StatusBadRequest {
			t.Errorf("%v: status %d", body, status)
		}
	}
	ts.register("taken", "")
}

func TestLoginLogoutAndBruteForceLimit(t *testing.T) {
	ts := start(t)
	ts.register("alice", "")
	c := ts.newClient()

	if status, _ := c.do(http.MethodGet, "/api/me", nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous /api/me: %d", status)
	}
	c.mustJSON(http.MethodPost, "/api/auth/login", map[string]string{"username": "alice", "password": password}, http.StatusOK, nil)
	var me store.User
	c.mustJSON(http.MethodGet, "/api/me", nil, http.StatusOK, &me)
	if me.Username != "alice" || !me.IsAdmin {
		t.Fatalf("me: %+v", me)
	}
	if status, data := c.do(http.MethodGet, "/api/me", nil); status != 200 || strings.Contains(string(data), "argon2") {
		t.Fatal("password hash leaked")
	}
	c.mustJSON(http.MethodPost, "/api/auth/logout", map[string]string{}, http.StatusNoContent, nil)
	if status, _ := c.do(http.MethodGet, "/api/me", nil); status != http.StatusUnauthorized {
		t.Fatal("still logged in after logout")
	}

	var last int
	for i := 0; i < 12; i++ {
		last, _ = c.do(http.MethodPost, "/api/auth/login", map[string]string{"username": "alice", "password": "wrong password here"})
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("no lockout, last status %d", last)
	}
}

func TestChangePasswordEndsOtherSessions(t *testing.T) {
	ts := start(t)
	first := ts.register("alice", "")
	second := ts.newClient()
	second.mustJSON(http.MethodPost, "/api/auth/login", map[string]string{"username": "alice", "password": password}, http.StatusOK, nil)

	first.mustJSON(http.MethodPost, "/api/me/password", map[string]string{"current": password, "new": "a different long password"}, http.StatusNoContent, nil)
	if status, _ := first.do(http.MethodGet, "/api/me", nil); status != http.StatusOK {
		t.Fatal("current session should survive")
	}
	if status, _ := second.do(http.MethodGet, "/api/me", nil); status != http.StatusUnauthorized {
		t.Fatal("other session should end")
	}
}

func TestWorkspaceIsolation(t *testing.T) {
	ts := start(t, func(c *Config) { c.Registration = RegistrationOpen })
	alice := ts.register("alice", "")
	bob := ts.register("bob", "")
	ws := alice.workspaceID()
	agentID, _ := alice.createAgent(ws, "frontend")

	paths := []struct{ method, path string }{
		{http.MethodGet, "/api/workspaces/" + ws + "/agents"},
		{http.MethodGet, "/api/workspaces/" + ws + "/messages"},
		{http.MethodGet, "/api/workspaces/" + ws + "/threads"},
		{http.MethodGet, "/api/workspaces/" + ws + "/members"},
		{http.MethodDelete, "/api/workspaces/" + ws + "/agents/" + strconv.FormatInt(agentID, 10)},
	}
	for _, p := range paths {
		if status, _ := bob.do(p.method, p.path, nil); status != http.StatusNotFound {
			t.Errorf("bob %s %s: status %d", p.method, p.path, status)
		}
	}
}

func TestMemberCannotManageOthers(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	var created struct {
		Code string `json:"code"`
	}
	owner.mustJSON(http.MethodPost, "/api/workspaces/"+ws+"/invites", map[string]int{"max_uses": 1}, http.StatusCreated, &created)
	member := ts.register("member", created.Code)
	agentID, _ := owner.createAgent(ws, "ownerbot")
	path := "/api/workspaces/" + ws + "/agents/" + strconv.FormatInt(agentID, 10)

	if status, _ := member.do(http.MethodDelete, path, nil); status != http.StatusForbidden {
		t.Fatalf("revoke: %d", status)
	}
	if status, _ := member.do(http.MethodGet, "/api/workspaces/"+ws+"/invites", nil); status != http.StatusForbidden {
		t.Fatalf("invites: %d", status)
	}
	if status, _ := owner.do(http.MethodDelete, path, nil); status != http.StatusNoContent {
		t.Fatalf("owner revoke: %d", status)
	}
}

func TestRequiresJSONContentType(t *testing.T) {
	ts := start(t)
	resp, err := http.Post(ts.url+"/api/auth/login", "application/x-www-form-urlencoded", strings.NewReader("username=a"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestCrossOriginPostIsRejected(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	req, _ := http.NewRequest(http.MethodPost, ts.url+"/api/workspaces", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := owner.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestDownloadRejectsTraversal(t *testing.T) {
	ts := start(t)
	for _, file := range []string{"..%2f..%2fetc%2fpasswd", "server.db", "agentic-chat-linux-amd64.sh"} {
		resp, err := http.Get(ts.url + "/downloads/" + file)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d", file, resp.StatusCode)
		}
	}
}

func TestInstallScriptUsesRequestHost(t *testing.T) {
	ts := start(t)
	resp, err := http.Get(ts.url + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), ts.url+"/downloads/") {
		t.Fatalf("script does not point back at this hub:\n%s", body)
	}
	if strings.Contains(string(body), "%!") {
		t.Fatalf("format error in script:\n%s", body)
	}
}

func dialAgent(t *testing.T, ts *testServer, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.url, "http")+"/api/agent/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func TestPushOverWebsocketAndRevoke(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	agentID, token := owner.createAgent(ws, "backend")

	conn := dialAgent(t, ts, token)
	var agents []agentView
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		owner.mustJSON(http.MethodGet, "/api/workspaces/"+ws+"/agents", nil, http.StatusOK, &agents)
		if stateOf(agents, "backend") == hub.StateOnline {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stateOf(agents, "backend") != hub.StateOnline {
		t.Fatalf("agents: %+v", agents)
	}

	// A person sends from the web UI, the agent gets the push.
	owner.mustJSON(http.MethodPost, "/api/workspaces/"+ws+"/messages", map[string]string{"to": "backend", "body": "ping from a person"}, http.StatusCreated, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var frame AgentFrame
	if err := wsjson.Read(ctx, conn, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "message" || frame.Message.Body != "ping from a person" || frame.Message.From != "owner" {
		t.Fatalf("frame: %+v", frame)
	}

	// Revoking ends the connection with a code the client recognises and kills the token.
	owner.mustJSON(http.MethodDelete, "/api/workspaces/"+ws+"/agents/"+strconv.FormatInt(agentID, 10), nil, http.StatusNoContent, nil)
	// A copy of the message can still be in the pipe, delivery is at least once.
	var err error
	for err == nil {
		_, _, err = conn.Read(ctx)
	}
	if websocket.CloseStatus(err) != closeRevoked {
		t.Fatalf("expected the revoked close code, got %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.url+"/api/agent/inbox", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token still works: %d", resp.StatusCode)
	}
}

func stateOf(agents []agentView, name string) string {
	for _, a := range agents {
		if a.Name == name {
			return a.State
		}
	}
	return ""
}

func TestWebsocketRejectsOtherOrigin(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.url, "http")+"/api/workspaces/"+ws+"/ws", &websocket.DialOptions{
		HTTPClient: owner.http,
		HTTPHeader: http.Header{"Origin": {"https://evil.example"}},
	})
	if err == nil {
		t.Fatal("cross origin websocket accepted")
	}
	if resp != nil && resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("cross origin websocket upgraded")
	}
}

func TestSecurityHeadersPresent(t *testing.T) {
	ts := start(t)
	resp, err := http.Get(ts.url + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
}

func dialWorkspace(t *testing.T, c *client, ws string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(c.ts.url, "http")+"/api/workspaces/"+ws+"/ws", &websocket.DialOptions{
		HTTPClient: c.http,
		HTTPHeader: http.Header{"Origin": {c.ts.url}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func TestRemovedMemberLosesLiveStreamAtOnce(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	var created struct {
		Code string `json:"code"`
	}
	owner.mustJSON(http.MethodPost, "/api/workspaces/"+ws+"/invites", map[string]int{"max_uses": 1}, http.StatusCreated, &created)
	guest := ts.register("guest", created.Code)
	conn := dialWorkspace(t, guest, ws)

	// Presence events flow while the guest is a member.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var members []store.Member
	owner.mustJSON(http.MethodGet, "/api/workspaces/"+ws+"/members", nil, http.StatusOK, &members)
	var guestID int64
	for _, m := range members {
		if m.Username == "guest" {
			guestID = m.UserID
		}
	}
	owner.mustJSON(http.MethodDelete, "/api/workspaces/"+ws+"/members/"+strconv.FormatInt(guestID, 10), nil, http.StatusNoContent, nil)

	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatal("the stream stayed open after the member was removed")
			}
			break
		}
	}
	if status, _ := guest.do(http.MethodGet, "/api/workspaces/"+ws+"/messages", nil); status != http.StatusNotFound {
		t.Fatalf("removed member can still read messages: %d", status)
	}
}

func TestPeerCannotReadOrConfirmAnothersMessages(t *testing.T) {
	ts := start(t, func(c *Config) { c.Registration = RegistrationOpen })
	alice := ts.register("alice", "")
	bob := ts.register("bob", "")
	aliceWs := alice.workspaceID()
	_, aliceToken := alice.createAgent(aliceWs, "secretbot")
	_, bobToken := bob.createAgent(bob.workspaceID(), "bobbot")

	var sent store.Message
	alice.mustJSON(http.MethodPost, "/api/workspaces/"+aliceWs+"/messages", map[string]string{"to": "secretbot", "body": "private plans"}, http.StatusCreated, &sent)

	// Bob's agent tries to confirm Alice's message as delivered.
	body, _ := json.Marshal(map[string]any{"ids": []int64{sent.ID}})
	req, _ := http.NewRequest(http.MethodPost, ts.url+"/api/agent/inbox/ack", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bobToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// The real recipient still has it unread, and only the recipient sees it.
	inbox := func(token string) string {
		req, _ := http.NewRequest(http.MethodGet, ts.url+"/api/agent/inbox?pending=1", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return string(data)
	}
	if !strings.Contains(inbox(aliceToken), "private plans") {
		t.Fatal("another agent was able to mark the message delivered")
	}
	if strings.Contains(inbox(bobToken), "private plans") {
		t.Fatal("message leaked to another workspace")
	}
}

func TestStandbyDialLeavesCurrentHolderAlone(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	_, token := owner.createAgent(ws, "backend")
	holder := dialAgent(t, ts, token)
	_ = holder

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		var conn *websocket.Conn
		conn, resp, err = websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.url, "http")+"/api/agent/ws?standby=1", &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
		})
		if err != nil {
			break
		}
		conn.CloseNow() // the holder had not registered yet, try again
		time.Sleep(20 * time.Millisecond)
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("standby dial should be refused, got %v %+v", err, resp)
	}
}

func TestCancelledInviteStopsWorking(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	var created struct {
		Code   string `json:"code"`
		Invite struct {
			ID int64 `json:"id"`
		} `json:"invite"`
	}
	owner.mustJSON(http.MethodPost, "/api/workspaces/"+ws+"/invites", map[string]int{"max_uses": 5}, http.StatusCreated, &created)
	var list []store.Invite
	owner.mustJSON(http.MethodGet, "/api/workspaces/"+ws+"/invites", nil, http.StatusOK, &list)
	if len(list) != 1 {
		t.Fatalf("invites: %+v", list)
	}
	owner.mustJSON(http.MethodDelete, "/api/workspaces/"+ws+"/invites/"+strconv.FormatInt(created.Invite.ID, 10), nil, http.StatusNoContent, nil)
	status, _ := ts.newClient().do(http.MethodPost, "/api/auth/register", map[string]string{"username": "late", "password": password, "invite": created.Code})
	if status != http.StatusBadRequest {
		t.Fatalf("cancelled invite: status %d", status)
	}
}

func TestExistingUserJoinsThroughInvite(t *testing.T) {
	ts := start(t, func(c *Config) { c.Registration = RegistrationOpen })
	owner := ts.register("owner", "")
	other := ts.register("other", "")
	ws := owner.workspaceID()
	var created struct {
		Code string `json:"code"`
	}
	owner.mustJSON(http.MethodPost, "/api/workspaces/"+ws+"/invites", map[string]int{"max_uses": 2}, http.StatusCreated, &created)
	other.mustJSON(http.MethodPost, "/api/invites/accept", map[string]string{"code": created.Code}, http.StatusOK, nil)
	if status, _ := other.do(http.MethodGet, "/api/workspaces/"+ws+"/messages", nil); status != http.StatusOK {
		t.Fatalf("joined member cannot read: %d", status)
	}
	// Joining twice is refused and does not use up a second place.
	if status, _ := other.do(http.MethodPost, "/api/invites/accept", map[string]string{"code": created.Code}); status != http.StatusConflict {
		t.Fatalf("second join: %d", status)
	}
}

func TestRemovingAMemberRevokesTheirAgents(t *testing.T) {
	ts := start(t)
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	var created struct {
		Code string `json:"code"`
	}
	owner.mustJSON(http.MethodPost, "/api/workspaces/"+ws+"/invites", map[string]int{"max_uses": 1}, http.StatusCreated, &created)
	guest := ts.register("guest", created.Code)
	_, token := guest.createAgent(ws, "guestbot")

	var members []store.Member
	owner.mustJSON(http.MethodGet, "/api/workspaces/"+ws+"/members", nil, http.StatusOK, &members)
	for _, m := range members {
		if m.Username == "guest" {
			owner.mustJSON(http.MethodDelete, "/api/workspaces/"+ws+"/members/"+strconv.FormatInt(m.UserID, 10), nil, http.StatusNoContent, nil)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, ts.url+"/api/agent/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("agent of a removed member still works: %d", resp.StatusCode)
	}
}

func TestDemoModeLimitsNewcomersOnly(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "demo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	serve := func(demo bool) *testServer {
		cfg := Config{DataDir: dir, DownloadsDir: dir, Registration: RegistrationOpen, Limits: hub.DefaultLimits(), Demo: demo}
		s.SetDemo(false)
		srv := httptest.NewServer(New(cfg, s, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
		t.Cleanup(srv.Close)
		return &testServer{t: t, url: srv.URL}
	}

	// The hub is in use before the flag goes on.
	before := serve(false)
	admin := before.register("admin", "")
	colleague := before.register("colleague", "")
	colleagueWs := colleague.workspaceID()

	ts := serve(true)
	var config map[string]any
	ts.newClient().mustJSON(http.MethodGet, "/api/config", nil, http.StatusOK, &config)
	if config["demo"] != true {
		t.Fatalf("config: %v", config)
	}

	visitor := ts.register("visitor", "")
	ws := visitor.workspaceID()
	var list []store.Workspace
	visitor.mustJSON(http.MethodGet, "/api/workspaces", nil, http.StatusOK, &list)
	if !list[0].Demo {
		t.Fatal("new workspace not marked as demo")
	}

	// A second workspace and a fourth agent are refused, with a reason.
	if status, body := visitor.do(http.MethodPost, "/api/workspaces", map[string]string{"name": "more"}); status != http.StatusForbidden || !strings.Contains(string(body), "one workspace") {
		t.Fatalf("second workspace: %d %s", status, body)
	}
	for _, name := range []string{"a1", "a2", "a3"} {
		visitor.createAgent(ws, name)
	}
	if status, body := visitor.do(http.MethodPost, "/api/workspaces/"+ws+"/agents", map[string]string{"name": "a4"}); status != http.StatusForbidden || !strings.Contains(string(body), "at most") {
		t.Fatalf("fourth agent: %d %s", status, body)
	}

	// Messages are capped in size, and the reason says how to get rid of the limit.
	big := strings.Repeat("x", demoMaxMessageBytes+1)
	if status, body := visitor.do(http.MethodPost, "/api/workspaces/"+ws+"/messages", map[string]string{"to": "a1", "body": big}); status != http.StatusBadRequest || !strings.Contains(string(body), "host your own hub") {
		t.Fatalf("big message: %d %s", status, body)
	}
	visitor.mustJSON(http.MethodPost, "/api/workspaces/"+ws+"/messages", map[string]string{"to": "a1", "body": "small"}, http.StatusCreated, nil)

	// Whoever was here before the flag has no limits, and neither do admins.
	colleague2 := ts.newClient()
	colleague2.mustJSON(http.MethodPost, "/api/auth/login", map[string]string{"username": "colleague", "password": password}, http.StatusOK, nil)
	for _, name := range []string{"b1", "b2", "b3", "b4"} {
		colleague2.createAgent(colleagueWs, name)
	}
	colleague2.mustJSON(http.MethodPost, "/api/workspaces/"+colleagueWs+"/messages", map[string]string{"to": "b1", "body": big}, http.StatusCreated, nil)
	admin2 := ts.newClient()
	admin2.mustJSON(http.MethodPost, "/api/auth/login", map[string]string{"username": "admin", "password": password}, http.StatusOK, nil)
	admin2.mustJSON(http.MethodPost, "/api/workspaces", map[string]string{"name": "extra"}, http.StatusCreated, nil)
	_ = admin
}

func TestDemoCapsSignupsPerAddress(t *testing.T) {
	ts := start(t, func(c *Config) { c.Registration = RegistrationOpen; c.Demo = true })
	ts.register("first", "")
	for i := 0; i < demoSignupsPerIP; i++ {
		ts.register("user"+strconv.Itoa(i), "")
	}
	status, _ := ts.newClient().do(http.MethodPost, "/api/auth/register", map[string]string{"username": "onemore", "password": password})
	if status != http.StatusTooManyRequests {
		t.Fatalf("status %d", status)
	}
}

func TestDemoOffChangesNothing(t *testing.T) {
	ts := start(t, func(c *Config) { c.Registration = RegistrationOpen })
	ts.register("first", "")
	u := ts.register("second", "")
	ws := u.workspaceID()
	var list []store.Workspace
	u.mustJSON(http.MethodGet, "/api/workspaces", nil, http.StatusOK, &list)
	if list[0].Demo {
		t.Fatal("marked as demo with the flag off")
	}
	for i := 0; i < demoMaxAgents+2; i++ {
		u.createAgent(ws, "agent"+strconv.Itoa(i))
	}
	u.mustJSON(http.MethodPost, "/api/workspaces", map[string]string{"name": "another"}, http.StatusCreated, nil)
}

func TestConnectionCapRefusesThenRecovers(t *testing.T) {
	ts := start(t, func(c *Config) { c.MaxConnections = 2 })
	owner := ts.register("owner", "")
	ws := owner.workspaceID()
	_, a := owner.createAgent(ws, "a")
	_, b := owner.createAgent(ws, "b")
	_, c := owner.createAgent(ws, "c")

	first := dialAgent(t, ts, a)
	dialAgent(t, ts, b)
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.url, "http")+"/api/agent/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c}},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("third connection: %v %+v", err, resp)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("refusal does not say when to retry")
	}
	// The web UI and health checks are not long lived and stay reachable.
	if status, _ := owner.do(http.MethodGet, "/api/workspaces/"+ws+"/agents", nil); status != http.StatusOK {
		t.Fatalf("normal api call refused: %d", status)
	}
	first.CloseNow()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.url, "http")+"/api/agent/ws", &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer " + c}},
		})
		if err == nil {
			conn.CloseNow()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("slot never came free")
}

func TestConnLimiterPerAddressAndOff(t *testing.T) {
	l := newConnLimiter(100)
	for i := 0; i < l.perIP; i++ {
		if !l.acquire("1.1.1.1") {
			t.Fatal("refused below the per address limit")
		}
	}
	if l.acquire("1.1.1.1") {
		t.Fatal("one address went over its share")
	}
	if !l.acquire("2.2.2.2") {
		t.Fatal("another address was refused")
	}
	l.release("1.1.1.1")
	if !l.acquire("1.1.1.1") {
		t.Fatal("released slot not usable")
	}
	off := newConnLimiter(0)
	for i := 0; i < 1000; i++ {
		if !off.acquire("1.1.1.1") {
			t.Fatal("zero should mean no limit")
		}
	}
}

func TestStaticFilesCompressedAndRevalidated(t *testing.T) {
	ts := start(t)
	// Go's client only hides compression when it asked for it itself.
	req, _ := http.NewRequest(http.MethodGet, ts.url+"/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("not compressed: %v", resp.Header)
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(gz)
	if !bytes.Contains(plain, []byte("renderAuth")) || len(body) >= len(plain) {
		t.Fatalf("%d bytes on the wire, %d plain", len(body), len(plain))
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("content type %q, a browser refuses modules without the right one", resp.Header.Get("Content-Type"))
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no etag")
	}

	again, _ := http.NewRequest(http.MethodGet, ts.url+"/app.js", nil)
	again.Header.Set("If-None-Match", etag)
	resp, err = http.DefaultClient.Do(again)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status %d", resp.StatusCode)
	}

	for path, want := range map[string]int{"/": 200, "/index.html": 200, "/style.css": 200, "/nope.js": 404, "/../internal/server/server.go": 404} {
		req, _ := http.NewRequest(http.MethodGet, ts.url+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", path, resp.StatusCode, want)
		}
	}
	resp, _ = http.Post(ts.url+"/", "text/plain", strings.NewReader("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("post: %d", resp.StatusCode)
	}
	font, _ := http.Get(ts.url + "/fonts/schibsted-grotesk-latin-wght-normal.woff2")
	font.Body.Close()
	if font.StatusCode != 200 || !strings.Contains(font.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("font: %d %v", font.StatusCode, font.Header)
	}
}
