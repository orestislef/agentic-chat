package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/orestislef/agentic-chat/internal/store"
)

// The room page needs no account: whoever has the link can see the room, remove a name
// from it or delete it. The link is the only secret.

func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	_, token, err := s.createRoom(r)
	if errors.Is(err, errTooManyRooms) {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"token": token, "link": s.baseURL(r) + "/r/" + token})
}

func (s *Server) roomFromPath(w http.ResponseWriter, r *http.Request) *store.Room {
	room, err := s.roomByToken(r.Context(), r.PathValue("token"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "this room does not exist anymore")
		return nil
	}
	if err != nil {
		s.internalError(w, r, err)
		return nil
	}
	return room
}

type memberView struct {
	store.Member
	State string `json:"state"`
}

// handleRoom returns who is in a room and what was said. With after, only what is newer.
func (s *Server) handleRoom(w http.ResponseWriter, r *http.Request) {
	room := s.roomFromPath(w, r)
	if room == nil {
		return
	}
	members, err := s.store.Members(r.Context(), room.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	views := make([]memberView, len(members))
	for i, m := range members {
		views[i] = memberView{Member: m, State: state(m)}
	}
	var messages []store.Message
	if after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64); after > 0 {
		messages, err = s.store.After(r.Context(), room.ID, after, 500)
	} else {
		messages, err = s.store.History(r.Context(), room.ID, 0, 300)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	link := s.baseURL(r) + "/r/" + r.PathValue("token")
	writeJSON(w, http.StatusOK, map[string]any{
		"members":  views,
		"messages": messages,
		"invite":   invite(s.baseURL(r), link),
	})
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	room := s.roomFromPath(w, r)
	if room == nil {
		return
	}
	err := s.store.RemoveMember(r.Context(), room.ID, r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "nobody by that name is in this room")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.waiters.wake(room.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteRoom(w http.ResponseWriter, r *http.Request) {
	room := s.roomFromPath(w, r)
	if room == nil {
		return
	}
	if err := s.store.DeleteRoom(r.Context(), room.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.waiters.wake(room.ID)
	w.WriteHeader(http.StatusNoContent)
}
