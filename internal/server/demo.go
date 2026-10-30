package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/orestislef/agentic-chat/internal/hub"
	"github.com/orestislef/agentic-chat/internal/store"
)

// Demo mode is for running a hub as a public try-out. It only applies to accounts and
// workspaces created while it is on, and never to admins, so it can be turned on for a
// hub that is already in use without touching anything that is there.
const (
	demoRetentionDays   = 3
	demoMaxUsers        = 1000
	demoSignupsPerIP    = 5
	demoMaxWorkspaces   = 1
	demoMaxAgents       = 3
	demoMaxMessageBytes = 8 * 1024
	demoMessagesPerDay  = 200
	demoSelfHostHint    = "host your own hub to remove this limit"
)

// demoQuota runs before a message is stored.
func (s *Server) demoQuota(ctx context.Context, workspaceID int64, bodyLen int) error {
	demo, err := s.store.IsDemoWorkspace(ctx, workspaceID)
	if err != nil || !demo {
		return err
	}
	if bodyLen > demoMaxMessageBytes {
		return fmt.Errorf("%w: demo messages can be %d bytes at most, send a shorter one or %s", hub.ErrLimitReached, demoMaxMessageBytes, demoSelfHostHint)
	}
	n, err := s.store.CountMessagesSince(ctx, workspaceID, time.Now().Add(-24*time.Hour).UnixMilli())
	if err != nil {
		return err
	}
	if n >= demoMessagesPerDay {
		return fmt.Errorf("%w: this demo workspace used its %d messages for today, %s", hub.ErrLimitReached, demoMessagesPerDay, demoSelfHostHint)
	}
	return nil
}

// demoSignupRefused checks the limits on new accounts and writes the refusal if any.
func (s *Server) demoSignupRefused(w http.ResponseWriter, r *http.Request, users int) bool {
	if !s.cfg.Demo || users == 0 {
		return false
	}
	if users >= demoMaxUsers {
		writeError(w, http.StatusForbidden, "the demo is full right now, try again in a few days or host your own hub")
		return true
	}
	if !s.signups.Allow(s.clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many new accounts from this address today")
		return true
	}
	return false
}

// demoWorkspaceRefused limits how many workspaces a demo account can make.
func (s *Server) demoWorkspaceRefused(w http.ResponseWriter, r *http.Request, u *store.User) bool {
	if !s.cfg.Demo || u.IsAdmin {
		return false
	}
	n, err := s.store.CountDemoWorkspaces(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return true
	}
	if n >= demoMaxWorkspaces {
		writeError(w, http.StatusForbidden, "demo accounts have one workspace, "+demoSelfHostHint)
		return true
	}
	return false
}

// demoAgentRefused limits how many agents a demo workspace can have.
func (s *Server) demoAgentRefused(w http.ResponseWriter, r *http.Request, workspaceID int64) bool {
	if !s.cfg.Demo {
		return false
	}
	demo, err := s.store.IsDemoWorkspace(r.Context(), workspaceID)
	if err == nil && demo {
		var n int
		if n, err = s.store.CountCodingAgents(r.Context(), workspaceID); err == nil && n >= demoMaxAgents {
			writeError(w, http.StatusForbidden, fmt.Sprintf("demo workspaces have %d agents at most, %s", demoMaxAgents, demoSelfHostHint))
			return true
		}
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return true
	}
	return false
}

func (s *Server) pruneDemo(ctx context.Context) {
	cutoff := time.Now().AddDate(0, 0, -demoRetentionDays).UnixMilli()
	workspaces, users, err := s.store.PruneDemo(ctx, cutoff)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("prune demo data", "err", err)
		}
		return
	}
	if workspaces > 0 || users > 0 {
		s.log.Info("pruned demo data", "workspaces", workspaces, "users", users)
	}
}

// maxChatAgents keeps a runaway client from filling a workspace with chats.
const maxChatAgents = 200

// agentLimit is how many agents a workspace can reach through chats joining.
func (s *Server) agentLimit(ctx context.Context, workspaceID int64) int {
	if s.cfg.Demo {
		if demo, err := s.store.IsDemoWorkspace(ctx, workspaceID); err == nil && demo {
			return demoMaxAgents
		}
	}
	return maxChatAgents
}
