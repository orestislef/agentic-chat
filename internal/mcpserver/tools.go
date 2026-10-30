package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/orestislef/agentic-chat/internal/hub"
	"github.com/orestislef/agentic-chat/internal/mcp"
	"github.com/orestislef/agentic-chat/internal/store"
)

// messageView is the shape messages take in tool results.
type messageView struct {
	MessageID int64  `json:"message_id"`
	ThreadID  int64  `json:"thread_id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Kind      string `json:"kind"`
	Body      string `json:"body"`
	InReplyTo int64  `json:"in_reply_to,omitempty"`
	SentAt    string `json:"sent_at"`
	// AlreadyPushed means this session may have seen it arrive by itself.
	AlreadyPushed bool `json:"already_pushed,omitempty"`
}

func view(m *store.Message) messageView {
	return messageView{
		MessageID: m.ID, ThreadID: m.ThreadID, From: m.From, To: m.To, Kind: m.Kind, Body: m.Body,
		InReplyTo: m.InReplyTo, SentAt: time.UnixMilli(m.CreatedAt).UTC().Format(time.RFC3339),
		AlreadyPushed: m.DeliveredAt != 0,
	}
}

func views(ms []store.Message) []messageView {
	out := make([]messageView, len(ms))
	for i := range ms {
		out[i] = view(&ms[i])
	}
	return out
}

// toolbox carries what the tool handlers need.
type toolbox struct {
	hub   *hub.Hub
	store *store.Store
}

// call runs a tool. Failures the model can act on come back as tool errors, not protocol errors.
func (t *toolbox) call(ctx context.Context, a *store.Agent, name string, args json.RawMessage) (any, error) {
	switch name {
	case "list_agents":
		return t.listAgents(ctx, a)
	case "ask_agent":
		return t.ask(ctx, a, args)
	case "send_message":
		return t.sendMessage(ctx, a, args)
	case "reply":
		return t.reply(ctx, a, args)
	case "wait_for_reply":
		return t.waitForReply(ctx, a, args)
	case "check_inbox":
		return t.checkInbox(ctx, a, args)
	case "get_history":
		return t.history(ctx, a, args)
	case "search_history":
		return t.search(ctx, a, args)
	}
	return nil, errUnknownTool
}

var errUnknownTool = errors.New("unknown tool")

func decode(args json.RawMessage, into any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, into); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}

// seconds turns an optional wait argument into a duration.
func seconds(v *int, fallback int) time.Duration {
	n := fallback
	if v != nil {
		n = *v
	}
	return time.Duration(clamp(n, 0, mcp.MaxWaitSeconds)) * time.Second
}

func (t *toolbox) listAgents(ctx context.Context, a *store.Agent) (any, error) {
	agents, err := t.store.Agents(ctx, a.WorkspaceID)
	if err != nil {
		return nil, err
	}
	type entry struct {
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		State       string `json:"state"`
		Description string `json:"description,omitempty"`
	}
	list := []entry{}
	for i := range agents {
		if agents[i].ID == a.ID {
			continue
		}
		list = append(list, entry{agents[i].Name, agents[i].Kind, t.hub.State(&agents[i]), agents[i].Description})
	}
	return map[string]any{"you": a.Name, "agents": list}, nil
}

func (t *toolbox) ask(ctx context.Context, a *store.Agent, args json.RawMessage) (any, error) {
	var in struct {
		To          string `json:"to"`
		Message     string `json:"message"`
		ThreadID    int64  `json:"thread_id"`
		WaitSeconds *int   `json:"wait_seconds"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	m, err := t.hub.Send(ctx, a, in.To, "ask", in.Message, in.ThreadID)
	if err != nil {
		return nil, err
	}
	target, err := t.store.AgentByID(ctx, a.WorkspaceID, m.ToID)
	if err != nil {
		return nil, err
	}
	if t.hub.State(target) == hub.StateOffline {
		return map[string]any{
			"status": "queued", "message_id": m.ID, "thread_id": m.ThreadID,
			"note": in.To + " is offline. They get the question when they connect, and the answer comes back to you then.",
		}, nil
	}
	return t.awaitReply(ctx, a, m.ID, m.ThreadID, seconds(in.WaitSeconds, mcp.DefaultWaitSeconds))
}

func (t *toolbox) awaitReply(ctx context.Context, a *store.Agent, messageID, threadID int64, wait time.Duration) (any, error) {
	reply, err := t.hub.WaitReply(ctx, a, messageID, wait)
	if errors.Is(err, hub.ErrRecipientGone) {
		return map[string]any{
			"status": "unreachable", "message_id": messageID, "thread_id": threadID,
			"note": "That agent was removed from the workspace, so this question will not be answered. Do not wait for it, tell your user.",
		}, nil
	}
	if err != nil {
		return nil, err
	}
	if reply == nil {
		return map[string]any{
			"status": "pending", "message_id": messageID, "thread_id": threadID,
			"note": "No answer yet. Carry on with your work and look again later with wait_for_reply or check_inbox, unless your instructions say messages reach you by themselves.",
		}, nil
	}
	return map[string]any{"status": "answered", "reply": view(reply)}, nil
}

func (t *toolbox) sendMessage(ctx context.Context, a *store.Agent, args json.RawMessage) (any, error) {
	var in struct {
		To       string `json:"to"`
		Message  string `json:"message"`
		ThreadID int64  `json:"thread_id"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	m, err := t.hub.Send(ctx, a, in.To, "note", in.Message, in.ThreadID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "sent", "message_id": m.ID, "thread_id": m.ThreadID}, nil
}

func (t *toolbox) reply(ctx context.Context, a *store.Agent, args json.RawMessage) (any, error) {
	var in struct {
		MessageID int64  `json:"message_id"`
		Message   string `json:"message"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	m, err := t.hub.Reply(ctx, a, in.MessageID, in.Message)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "sent", "message_id": m.ID, "thread_id": m.ThreadID}, nil
}

func (t *toolbox) waitForReply(ctx context.Context, a *store.Agent, args json.RawMessage) (any, error) {
	var in struct {
		MessageID   int64 `json:"message_id"`
		WaitSeconds *int  `json:"wait_seconds"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	original, err := t.store.MessageByID(ctx, a.WorkspaceID, in.MessageID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("message %d not found", in.MessageID)
	}
	if err != nil {
		return nil, err
	}
	return t.awaitReply(ctx, a, in.MessageID, original.ThreadID, seconds(in.WaitSeconds, mcp.DefaultWaitSeconds))
}

func (t *toolbox) checkInbox(ctx context.Context, a *store.Agent, args json.RawMessage) (any, error) {
	var in struct {
		Limit       int  `json:"limit"`
		WaitSeconds *int `json:"wait_seconds"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	limit := 20
	if in.Limit > 0 {
		limit = clamp(in.Limit, 1, 100)
	}
	messages, err := t.hub.Inbox(ctx, a, hub.InboxOptions{Limit: limit, Wait: seconds(in.WaitSeconds, 0)})
	if err != nil {
		return nil, err
	}
	return map[string]any{"messages": views(messages)}, nil
}

func (t *toolbox) history(ctx context.Context, a *store.Agent, args json.RawMessage) (any, error) {
	var in struct {
		ThreadID int64  `json:"thread_id"`
		With     string `json:"with"`
		BeforeID int64  `json:"before_id"`
		Limit    int    `json:"limit"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	filter := store.HistoryFilter{ThreadID: in.ThreadID, BeforeID: in.BeforeID, Limit: 30}
	if in.Limit > 0 {
		filter.Limit = clamp(in.Limit, 1, 200)
	}
	if in.With != "" {
		other, err := t.store.AgentByName(ctx, a.WorkspaceID, in.With)
		if errors.Is(err, store.ErrNotFound) {
			return nil, hub.ErrUnknownAgent
		}
		if err != nil {
			return nil, err
		}
		filter.AgentID, filter.WithID = a.ID, other.ID
	}
	messages, err := t.store.History(ctx, a.WorkspaceID, filter)
	if err != nil {
		return nil, err
	}
	return map[string]any{"messages": views(messages)}, nil
}

func (t *toolbox) search(ctx context.Context, a *store.Agent, args json.RawMessage) (any, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	limit := 20
	if in.Limit > 0 {
		limit = clamp(in.Limit, 1, 100)
	}
	messages, err := t.store.Search(ctx, a.WorkspaceID, in.Query, limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"messages": views(messages)}, nil
}
