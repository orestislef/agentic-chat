package server

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/orestislef/agentic-chat/internal/auth"
	"github.com/orestislef/agentic-chat/internal/mcpserver"
	"github.com/orestislef/agentic-chat/internal/store"
)

// Rooms are the simple way in: create one, send the link, everyone's chats join the same pool.
// There are no accounts. Whoever has the link is in, so the link is the only secret.
//
// Rooms sit on top of workspaces and connections. The address a client is given carries the room
// and a random id for that machine, and is turned into a connection token here, so the MCP endpoint
// handles it like any other signed in client.

var deviceRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// handleCreateRoom needs no login on a hub with open registration. Otherwise whoever runs the hub
// decides who can use it, and creating a room takes an account like everything else.
func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Registration != RegistrationOpen {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			writeError(w, http.StatusForbidden, "this hub needs a login to create a room")
			return
		}
		if _, err := s.store.UserBySession(r.Context(), auth.HashToken(cookie.Value)); err != nil {
			writeError(w, http.StatusForbidden, "this hub needs a login to create a room")
			return
		}
	}
	token, err := s.newRoom(r)
	if errors.Is(err, errTooManyRooms) {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"token": token})
}

// createRoomForChat is the create_room tool. It follows the same rules as the front page: on a hub
// without open registration it takes an account, which agent tokens and shared connections are not,
// because those can be in the hands of someone the hub's owner never let in.
func (s *Server) createRoomForChat(r *http.Request, hasAccount bool) (string, error) {
	if s.cfg.Registration != RegistrationOpen && !hasAccount {
		return "", errors.New("this hub needs an account to create a room and this connection has none. Tell your user, they can create one on the hub's front page")
	}
	base := s.hubURL(r)
	if base == "" {
		return "", errors.New("could not work out this hub's address. Tell your user, they can create a room on the hub's front page")
	}
	token, err := s.newRoom(r)
	if errors.Is(err, errTooManyRooms) {
		return "", err
	}
	if err != nil {
		s.log.Error("create room", "err", err)
		return "", errors.New("could not create the room, try again")
	}
	return base + "/r/" + token, nil
}

var errTooManyRooms = errors.New("too many new rooms from this address, try again later")

// newRoom creates a room and returns its link token. Callers decide who may do this.
func (s *Server) newRoom(r *http.Request) (string, error) {
	if !s.roomCreates.Allow(s.clientIP(r)) {
		return "", errTooManyRooms
	}
	token, err := auth.NewLinkToken()
	if err != nil {
		return "", err
	}
	// The ~ cannot appear in a real username, so this name can never collide with or log in as one.
	if _, err := s.store.CreateRoom(r.Context(), auth.HashToken(token), "~room-"+auth.HashToken(token)[:16]); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Server) roomFromPath(w http.ResponseWriter, r *http.Request) *store.Room {
	room, err := s.store.RoomByToken(r.Context(), auth.HashToken(r.PathValue("token")))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "this room does not exist anymore, ask for a new link")
		return nil
	}
	if err != nil {
		s.internalError(w, r, err)
		return nil
	}
	return room
}

// handleRoomMCP turns a room address into a connection. It answers 404 for an unknown room rather
// than 401, because a 401 would send the client off to a browser sign in that does not apply here.
func (s *Server) handleRoomMCP(mcp http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		device := r.PathValue("device")
		if device != "" && !deviceRE.MatchString(device) {
			writeError(w, http.StatusNotFound, "not a valid room address")
			return
		}
		room := s.roomFromPath(w, r)
		if room == nil {
			return
		}
		token := mcpserver.ConnectionTokenPrefix + r.PathValue("token") + "/" + device
		if r.Method == http.MethodPost {
			err := s.store.EnsureRoomConnection(r.Context(), room, auth.HashToken(token))
			if errors.Is(err, store.ErrAgentLimit) {
				writeError(w, http.StatusForbidden, "this room is full")
				return
			}
			if err != nil {
				s.internalError(w, r, err)
				return
			}
		}
		r.Header.Set("Authorization", "Bearer "+token)
		mcp(w, r)
	}
}

func (s *Server) handleRoomPage(w http.ResponseWriter, r *http.Request) {
	s.static.serveFile(w, r, "index.html")
}

func (s *Server) handleRoomState(w http.ResponseWriter, r *http.Request) {
	room := s.roomFromPath(w, r)
	if room == nil {
		return
	}
	agents, err := s.store.Agents(r.Context(), room.WorkspaceID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	views := make([]agentView, len(agents))
	for i := range agents {
		views[i] = agentView{Agent: agents[i], State: s.hub.State(&agents[i])}
	}
	threads, err := s.store.Threads(r.Context(), room.WorkspaceID, queryInt(r, "limit", 50, 1, 1000))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": views, "threads": threads})
}

func (s *Server) handleRoomMessages(w http.ResponseWriter, r *http.Request) {
	room := s.roomFromPath(w, r)
	if room == nil {
		return
	}
	messages, err := s.store.History(r.Context(), room.WorkspaceID, store.HistoryFilter{
		ThreadID: int64(queryInt(r, "thread", 0, 0, int(^uint32(0)>>1))),
		Limit:    queryInt(r, "limit", 200, 1, 500),
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, messages)
}

func (s *Server) handleRoomRemoveAgent(w http.ResponseWriter, r *http.Request) {
	room := s.roomFromPath(w, r)
	if room == nil {
		return
	}
	id, ok := pathID(r, "agentID")
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	agent, err := s.store.AgentByID(r.Context(), room.WorkspaceID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.store.RevokeAgent(r.Context(), room.WorkspaceID, agent.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.hub.Kick(room.WorkspaceID, agent.ID)
	s.hub.AgentGone(r.Context(), agent)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteRoom(w http.ResponseWriter, r *http.Request) {
	room := s.roomFromPath(w, r)
	if room == nil {
		return
	}
	agents, err := s.store.Agents(r.Context(), room.WorkspaceID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.store.DeleteRoom(r.Context(), room); err != nil {
		s.internalError(w, r, err)
		return
	}
	for i := range agents {
		s.hub.Kick(room.WorkspaceID, agents[i].ID)
	}
	s.log.Info("room deleted", "workspace", room.WorkspaceID)
	w.WriteHeader(http.StatusNoContent)
}
