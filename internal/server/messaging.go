package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/orestislef/agentic-chat/internal/auth"
	"github.com/orestislef/agentic-chat/internal/hub"
	"github.com/orestislef/agentic-chat/internal/store"
)

const (
	maxDescription = 200
	pingInterval   = 30 * time.Second
	writeTimeout   = 10 * time.Second

	// inboxWaitCap stays below the 60 second default read timeout of common
	// reverse proxies. Clients that want to wait longer call again.
	inboxWaitCap = 50

	// Websocket close codes that tell the client what to do next.
	closeSuperseded = websocket.StatusCode(4001)
	closeOverflow   = websocket.StatusCode(4002)
	closeRevoked    = websocket.StatusCode(4003)
)

// agentView adds the live presence state to an agent.
type agentView struct {
	store.Agent
	State string `json:"state"`
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request, c *memberContext) {
	agents, err := s.store.Agents(r.Context(), c.WorkspaceID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]agentView, len(agents))
	for i := range agents {
		out[i] = agentView{Agent: agents[i], State: s.hub.State(&agents[i])}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request, c *memberContext) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if !hub.ValidName(name) {
		writeError(w, http.StatusBadRequest, "name must be up to 32 characters: lowercase letters, digits, - and _, starting with a letter or digit")
		return
	}
	description := strings.TrimSpace(in.Description)
	if len(description) > maxDescription {
		writeError(w, http.StatusBadRequest, "description is too long")
		return
	}
	token, err := auth.NewToken("ac")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	agent, err := s.store.CreateAgent(r.Context(), c.WorkspaceID, c.User.ID, name, description, auth.HashToken(token))
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "that name is taken in this workspace")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.hub.AgentsChanged(c.WorkspaceID)
	writeJSON(w, http.StatusCreated, map[string]any{"agent": agentView{Agent: *agent, State: hub.StateOffline}, "token": token})
}

// manageableAgent loads the agent in the route and checks the caller may change it.
func (s *Server) manageableAgent(w http.ResponseWriter, r *http.Request, c *memberContext) (*store.Agent, bool) {
	id, ok := pathID(r, "agentID")
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found")
		return nil, false
	}
	agent, err := s.store.AgentByID(r.Context(), c.WorkspaceID, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "agent not found")
		return nil, false
	}
	if agent.Kind != "agent" {
		writeError(w, http.StatusBadRequest, "that entry is a person, remove them from the members list instead")
		return nil, false
	}
	if c.Role != "owner" && agent.UserID != c.User.ID {
		writeError(w, http.StatusForbidden, "only its creator or a workspace owner can change this agent")
		return nil, false
	}
	return agent, true
}

func (s *Server) handleRevokeAgent(w http.ResponseWriter, r *http.Request, c *memberContext) {
	agent, ok := s.manageableAgent(w, r, c)
	if !ok {
		return
	}
	if err := s.store.RevokeAgent(r.Context(), c.WorkspaceID, agent.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.hub.Kick(c.WorkspaceID, agent.ID)
	s.hub.AgentGone(r.Context(), agent)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRotateAgent(w http.ResponseWriter, r *http.Request, c *memberContext) {
	agent, ok := s.manageableAgent(w, r, c)
	if !ok {
		return
	}
	token, err := auth.NewToken("ac")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.store.RotateAgentToken(r.Context(), c.WorkspaceID, agent.ID, auth.HashToken(token)); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.hub.Kick(c.WorkspaceID, agent.ID)
	writeJSON(w, http.StatusOK, map[string]any{"token": token})
}

func (s *Server) handleListThreads(w http.ResponseWriter, r *http.Request, c *memberContext) {
	before := int64(queryInt(r, "before", 0, 0, int(^uint32(0)>>1)))
	threads, err := s.store.Threads(r.Context(), c.WorkspaceID, before, queryInt(r, "limit", 50, 1, 200))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, threads)
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request, c *memberContext) {
	limit := queryInt(r, "limit", 100, 1, 500)
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		hits, err := s.store.Search(r.Context(), c.WorkspaceID, q, limit)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, hits)
		return
	}
	messages, err := s.store.History(r.Context(), c.WorkspaceID, store.HistoryFilter{
		ThreadID: int64(queryInt(r, "thread", 0, 0, int(^uint32(0)>>1))),
		BeforeID: int64(queryInt(r, "before", 0, 0, int(^uint32(0)>>1))),
		Limit:    limit,
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, messages)
}

// handleSendMessage lets a person take part in a conversation from the web UI.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request, c *memberContext) {
	var in struct {
		To        string `json:"to"`
		Body      string `json:"body"`
		ThreadID  int64  `json:"thread_id"`
		InReplyTo int64  `json:"in_reply_to"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	me, err := s.store.HumanAgent(r.Context(), c.WorkspaceID, c.User.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var m *store.Message
	if in.InReplyTo != 0 {
		m, err = s.hub.Reply(r.Context(), me, in.InReplyTo, in.Body)
	} else {
		m, err = s.hub.Send(r.Context(), me, in.To, "ask", in.Body, in.ThreadID)
	}
	if err != nil {
		s.writeSendError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

// writeSendError maps hub failures to status codes. Every expected hub error is
// meant to be read, so anything that is not a wrapped database failure goes out as is.
func (s *Server) writeSendError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, hub.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, hub.ErrUnknownAgent), errors.Is(err, hub.ErrUnknownThread):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, hub.ErrNotRecipient):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, hub.ErrEmptyBody), errors.Is(err, hub.ErrSelfMessage), errors.Is(err, hub.ErrLimitReached):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		s.internalError(w, r, err)
	}
}

// handleWorkspaceSocket streams live events to the web UI. The library rejects
// upgrades from another origin, which is what makes the session cookie safe here.
func (s *Server) handleWorkspaceSocket(w http.ResponseWriter, r *http.Request, c *memberContext) {
	me, err := s.store.HumanAgent(r.Context(), c.WorkspaceID, c.User.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(context.Background())

	sub := s.hub.Subscribe(c.WorkspaceID, me.ID)
	defer s.hub.Unsubscribe(sub)

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case e := <-sub.C:
			if err := writeFrame(ctx, conn, e); err != nil {
				return
			}
		case <-sub.Closed:
			conn.Close(closeOverflow, "too slow, reconnect")
			return
		case <-ping.C:
			if err := pingConn(ctx, conn); err != nil {
				return
			}
			// A logout or an expired session must end the stream too, not only the
			// next API call. Access is checked again while the socket stays open.
			if !s.stillMember(ctx, r, c) {
				conn.Close(websocket.StatusPolicyViolation, "log in again")
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) stillMember(ctx context.Context, r *http.Request, c *memberContext) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	u, err := s.store.UserBySession(ctx, auth.HashToken(cookie.Value))
	if err != nil {
		return false
	}
	_, err = s.store.MemberRole(ctx, c.WorkspaceID, u.ID)
	return err == nil
}

func writeFrame(ctx context.Context, conn *websocket.Conn, v any) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return wsjson.Write(ctx, conn, v)
}

func pingConn(ctx context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Ping(ctx)
}

// AgentFrame is what travels over the agent websocket, in both directions.
type AgentFrame struct {
	Type    string         `json:"type"` // "message" or "notice" from the hub, "ack" from the client
	Message *store.Message `json:"message,omitempty"`
	ID      int64          `json:"id,omitempty"`
}

// handleAgentSocket is the live push connection of a bridge.
func (s *Server) handleAgentSocket(w http.ResponseWriter, r *http.Request, a *store.Agent) {
	// A client that was replaced earlier asks with standby=1: it takes the slot when
	// it is free and leaves the current holder alone when it is not.
	if r.URL.Query().Get("standby") == "1" && s.hub.Connected(a.ID) {
		writeError(w, http.StatusConflict, "another session holds this agent")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4096)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	live, err := s.hub.Attach(ctx, a)
	if err != nil {
		s.log.Error("attach agent", "agent", a.Name, "err", err)
		conn.Close(websocket.StatusInternalError, "try again")
		return
	}
	defer s.hub.Detach(a, live)
	s.log.Info("agent connected", "agent", a.Name, "workspace", a.WorkspaceID)
	defer s.log.Info("agent disconnected", "agent", a.Name, "workspace", a.WorkspaceID)

	// Reads run on their own so acks and pongs are handled while we wait on the hub.
	go func() {
		defer cancel()
		for {
			var frame AgentFrame
			if err := wsjson.Read(ctx, conn, &frame); err != nil {
				return
			}
			if frame.Type == "ack" && frame.ID != 0 {
				if err := s.hub.Ack(ctx, a.ID, frame.ID); err != nil && ctx.Err() == nil {
					s.log.Warn("ack", "agent", a.Name, "err", err)
				}
			}
		}
	}()

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case m := <-live.C:
			frame := AgentFrame{Type: "message", Message: &m}
			if m.ID == 0 { // from the hub itself, not stored and not acknowledged
				frame.Type = "notice"
			}
			if err := writeFrame(ctx, conn, frame); err != nil {
				return
			}
		case <-live.Closed:
			conn.Close(closeCode(live.Reason), live.Reason)
			return
		case <-ping.C:
			if err := pingConn(ctx, conn); err != nil {
				return
			}
			if err := s.store.TouchAgent(ctx, a.ID); err != nil && ctx.Err() == nil {
				s.log.Warn("touch agent", "err", err)
			}
			if err := s.hub.Redeliver(ctx, a, live); err != nil && ctx.Err() == nil {
				s.log.Warn("redeliver", "agent", a.Name, "err", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func closeCode(reason string) websocket.StatusCode {
	switch reason {
	case hub.ReasonSuperseded:
		return closeSuperseded
	case hub.ReasonRevoked:
		return closeRevoked
	}
	return closeOverflow
}

// handleAgentMe lets a client check its token without touching the inbox.
func (s *Server) handleAgentMe(w http.ResponseWriter, r *http.Request, a *store.Agent) {
	writeJSON(w, http.StatusOK, a)
}

// handleAgentInbox serves the hook command: unread messages, optionally waiting
// for one. It reports a replaced watcher with 409 so the old process can exit.
func (s *Server) handleAgentInbox(w http.ResponseWriter, r *http.Request, a *store.Agent) {
	messages, err := s.hub.Inbox(r.Context(), a, hub.InboxOptions{
		Limit:     queryInt(r, "limit", 20, 1, 100),
		Wait:      time.Duration(queryInt(r, "wait", 0, 0, inboxWaitCap)) * time.Second,
		Exclusive: r.URL.Query().Get("exclusive") == "1",
		Pending:   r.URL.Query().Get("pending") == "1",
	})
	if errors.Is(err, hub.ErrWatcherReplaced) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

// handleAgentInboxAck confirms messages fetched with pending=1.
func (s *Server) handleAgentInboxAck(w http.ResponseWriter, r *http.Request, a *store.Agent) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.IDs) > 100 {
		writeError(w, http.StatusBadRequest, "confirm at most 100 messages at once")
		return
	}
	if err := s.hub.Ack(r.Context(), a.ID, in.IDs...); err != nil {
		s.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
