package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/orestislef/agentic-chat/internal/auth"
	"github.com/orestislef/agentic-chat/internal/hub"
	"github.com/orestislef/agentic-chat/internal/store"
	"github.com/orestislef/agentic-chat/internal/version"
)

const maxWorkspaceName = 64

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.CountUsers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":      version.Version,
		"registration": s.cfg.Registration,
		"first_user":   users == 0,
		"demo":         s.cfg.Demo,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username  string `json:"username"`
		Password  string `json:"password"`
		Invite    string `json:"invite"`
		Workspace string `json:"workspace"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !s.limits.Allow("register:" + s.clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, try again in a few minutes")
		return
	}
	username := strings.ToLower(strings.TrimSpace(in.Username))
	if !hub.ValidName(username) || len(username) < 2 {
		writeError(w, http.StatusBadRequest, "username must be 2 to 32 characters: lowercase letters, digits, - and _, starting with a letter or digit")
		return
	}
	if err := auth.ValidatePassword(in.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	invite := strings.TrimSpace(in.Invite)

	users, err := s.store.CountUsers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if users > 0 {
		switch {
		case s.cfg.Registration == RegistrationClosed:
			writeError(w, http.StatusForbidden, "this hub does not accept new accounts")
			return
		case s.cfg.Registration == RegistrationInvite && invite == "":
			writeError(w, http.StatusForbidden, "this hub needs an invite to register, ask a workspace owner for one")
			return
		}
	}

	if s.demoSignupRefused(w, r, users) {
		return
	}

	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	inviteHash := ""
	if invite != "" {
		inviteHash = auth.HashToken(invite)
	}
	workspace := strings.TrimSpace(in.Workspace)
	if workspace == "" {
		workspace = username
	}
	if len(workspace) > maxWorkspaceName {
		writeError(w, http.StatusBadRequest, "workspace name is too long")
		return
	}
	u, err := s.store.CreateAccount(r.Context(), username, hash, inviteHash, workspace)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "that username is taken")
		return
	case errors.Is(err, store.ErrInviteInvalid):
		writeError(w, http.StatusBadRequest, "that invite is expired, used up or wrong, ask for a new one")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	username := strings.ToLower(strings.TrimSpace(in.Username))
	key := "login:" + s.clientIP(r) + ":" + username
	if !s.limits.Allow(key) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, try again in a few minutes")
		return
	}
	if len(in.Password) > auth.MaxPasswordLength {
		writeError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	u, err := s.store.UserByUsername(r.Context(), username)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, err)
			return
		}
		auth.DummyVerify(in.Password)
		writeError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	if !auth.VerifyPassword(u.PasswordHash, in.Password) {
		writeError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	s.limits.Reset(key)
	if err := s.startSession(w, r, u.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err := s.store.DeleteSession(r.Context(), auth.HashToken(cookie.Value)); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	s.setSessionCookie(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token, err := auth.NewToken("acs")
	if err != nil {
		return err
	}
	expires := time.Now().Add(sessionLifetime)
	if err := s.store.CreateSession(r.Context(), auth.HashToken(token), userID, expires.UnixMilli()); err != nil {
		return err
	}
	s.setSessionCookie(w, r, token, int(sessionLifetime.Seconds()))
	return nil
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	key := "password:" + s.clientIP(r) + ":" + u.Username
	if !s.limits.Allow(key) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, try again in a few minutes")
		return
	}
	if len(in.Current) > auth.MaxPasswordLength || !auth.VerifyPassword(u.PasswordHash, in.Current) {
		writeError(w, http.StatusForbidden, "current password is wrong")
		return
	}
	if err := auth.ValidatePassword(in.New); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(in.New)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	keep := ""
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		keep = auth.HashToken(cookie.Value)
	}
	if err := s.store.SetPassword(r.Context(), u.ID, hash, keep); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.limits.Reset(key)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request, u *store.User) {
	list, err := s.store.WorkspacesForUser(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > maxWorkspaceName {
		writeError(w, http.StatusBadRequest, "workspace name must be 1 to 64 characters")
		return
	}
	if s.demoWorkspaceRefused(w, r, u) {
		return
	}
	created, err := s.store.CreateWorkspace(r.Context(), name, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleAcceptInvite(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !s.limits.Allow("invite:" + s.clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, try again in a few minutes")
		return
	}
	id, err := s.store.AcceptInvite(r.Context(), auth.HashToken(strings.TrimSpace(in.Code)), u)
	switch {
	case errors.Is(err, store.ErrInviteInvalid):
		writeError(w, http.StatusBadRequest, "that invite is expired, used up or wrong, ask for a new one")
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "you are already in that workspace")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"workspace_id": id})
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request, c *memberContext) {
	list, err := s.store.Members(r.Context(), c.WorkspaceID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleRemoveMember lets an owner remove anyone and anyone remove themselves.
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request, c *memberContext) {
	target, ok := pathID(r, "userID")
	if !ok {
		writeError(w, http.StatusNotFound, "member not found")
		return
	}
	if c.Role != "owner" && target != c.User.ID {
		writeError(w, http.StatusForbidden, "only a workspace owner can remove other members")
		return
	}
	agents, err := s.store.Agents(r.Context(), c.WorkspaceID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	err = s.store.RemoveMember(r.Context(), c.WorkspaceID, target)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "member not found")
		return
	case errors.Is(err, store.ErrLastOwner):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	if err := s.store.DeleteMemberConnections(r.Context(), c.WorkspaceID, target); err != nil {
		s.log.Warn("delete member connections", "err", err)
	}
	for i := range agents {
		if agents[i].UserID == target {
			s.hub.Kick(c.WorkspaceID, agents[i].ID)
			s.hub.AgentGone(r.Context(), &agents[i])
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request, c *memberContext) {
	list, err := s.store.Invites(r.Context(), c.WorkspaceID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request, c *memberContext) {
	var in struct {
		MaxUses int `json:"max_uses"`
		Days    int `json:"days"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	maxUses := max(1, min(in.MaxUses, 100))
	days := 7
	if in.Days > 0 {
		days = min(in.Days, 90)
	}
	code, err := auth.NewToken("aci")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	invite, err := s.store.CreateInvite(r.Context(), c.WorkspaceID, c.User.ID, auth.HashToken(code), maxUses,
		time.Now().AddDate(0, 0, days).UnixMilli())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"invite": invite, "code": code})
}

func (s *Server) handleDeleteInvite(w http.ResponseWriter, r *http.Request, c *memberContext) {
	id, ok := pathID(r, "inviteID")
	if !ok {
		writeError(w, http.StatusNotFound, "invite not found")
		return
	}
	err := s.store.DeleteInvite(r.Context(), c.WorkspaceID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "invite not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
