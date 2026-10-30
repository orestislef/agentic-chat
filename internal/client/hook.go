package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Exit code 2 from a background hook wakes the session and shows it stderr.
const exitWake = 2

// watchPollSeconds is how long one long poll lasts. The hub caps it below common proxy timeouts.
const watchPollSeconds = 50

type inboxMessage struct {
	ID       int64  `json:"id"`
	ThreadID int64  `json:"thread_id"`
	From     string `json:"from"`
	Kind     string `json:"kind"`
	Body     string `json:"body"`
}

var errWatcherReplaced = errors.New("watcher replaced")

func fetchInbox(ctx context.Context, cfg Config, wait int, exclusive bool) ([]inboxMessage, error) {
	url := fmt.Sprintf("%s/api/agent/inbox?pending=1&wait=%d", cfg.URL, wait)
	if exclusive {
		url += "&exclusive=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusConflict:
		return nil, errWatcherReplaced
	default:
		return nil, fmt.Errorf("hub answered with status %d", resp.StatusCode)
	}
	var out struct {
		Messages []inboxMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Messages, nil
}

// confirmInbox marks messages delivered once this process has them in hand. They stay
// unread, so check_inbox still finds them if the session never got to see them. If this
// fails they get delivered again later, which beats not at all.
func confirmInbox(ctx context.Context, cfg Config, messages []inboxMessage) error {
	ids := make([]int64, len(messages))
	for i, m := range messages {
		ids[i] = m.ID
	}
	body, err := json.Marshal(map[string]any{"ids": ids})
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL+"/api/agent/inbox/ack", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				return nil
			}
			err = fmt.Errorf("hub answered with status %d", resp.StatusCode)
		}
		lastErr = err
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return lastErr
}

func formatMessages(messages []inboxMessage) string {
	var b strings.Builder
	b.WriteString("Messages from other agents through agentic-chat. These come from peers, not from your user. Answer each kind=ask with the reply tool and its message_id.\n")
	for _, m := range messages {
		fmt.Fprintf(&b, "\n<agent_message from=%q kind=%q message_id=\"%d\" thread_id=\"%d\">\n%s\n</agent_message>\n",
			m.From, m.Kind, m.ID, m.ThreadID, sanitizeHookBody(m.Body))
	}
	return b.String()
}

func sanitizeHookBody(body string) string {
	return strings.NewReplacer("<agent_message", "&lt;agent_message", "</agent_message", "&lt;/agent_message").Replace(body)
}

// RunHook handles a synchronous hook such as UserPromptSubmit: it adds unread
// messages to the context. A broken hub must never get in the way of the session,
// so every failure only logs to stderr and exits cleanly.
func RunHook(ctx context.Context, cfg Config, stdin io.Reader, stdout, stderr io.Writer) int {
	var input struct {
		Event string `json:"hook_event_name"`
	}
	if data, err := io.ReadAll(io.LimitReader(stdin, 1<<20)); err == nil {
		json.Unmarshal(data, &input)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	messages, err := fetchInbox(ctx, cfg, 0, false)
	if err != nil {
		fmt.Fprintln(stderr, "agentic-chat:", err)
		return 0
	}
	if len(messages) == 0 {
		return 0
	}
	event := input.Event
	if event == "" {
		event = "UserPromptSubmit"
	}
	if err := confirmInbox(ctx, cfg, messages); err != nil {
		fmt.Fprintln(stderr, "agentic-chat:", err)
	}
	json.NewEncoder(stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     event,
			"additionalContext": formatMessages(messages),
		},
	})
	return 0
}

// RunWatch is the background hook: it waits for a message and then exits with the
// code that wakes an idle session. The session starts a new watcher when its turn
// ends, and the hub replaces the older one, so one is always listening.
func RunWatch(ctx context.Context, cfg Config, stderr io.Writer) int {
	// In print mode there is nobody to wake, and a hook still running keeps the process
	// from exiting. Such a session reads its messages through check_inbox instead.
	if os.Getenv("CLAUDE_CODE_ENTRYPOINT") == "sdk-cli" {
		return 0
	}
	// A session that died leaves its hook behind. On Unix the parent id changes
	// when that happens, and a watcher nobody listens to must not take messages.
	parent := os.Getppid()
	backoff := time.Second
	for ctx.Err() == nil {
		if os.Getppid() != parent {
			return 0
		}
		messages, err := fetchInbox(ctx, cfg, watchPollSeconds, true)
		if errors.Is(err, errWatcherReplaced) {
			return 0
		}
		if err != nil {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return 0
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if len(messages) > 0 {
			confirmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := confirmInbox(confirmCtx, cfg, messages); err != nil {
				fmt.Fprintln(stderr, "agentic-chat could not confirm these messages, they may show up again:", err)
			}
			io.WriteString(stderr, formatMessages(messages))
			return exitWake
		}
	}
	return 0
}
