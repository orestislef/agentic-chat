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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Delivery modes for setup.
const (
	ModeChannel = "channel"
	ModeHook    = "hook"
)

const serverName = "agentic-chat"

// watchHookTimeout is how long one background watcher may live, in seconds. The
// session starts a new one at the end of each turn.
const watchHookTimeout = 86400

// SetupOptions are the arguments of the setup command.
type SetupOptions struct {
	Config
	Mode string
	Dir  string
}

// Setup connects the project in opts.Dir to the hub.
func Setup(ctx context.Context, opts SetupOptions, out io.Writer) error {
	if opts.Mode != ModeChannel && opts.Mode != ModeHook {
		return fmt.Errorf("--mode must be %s or %s", ModeChannel, ModeHook)
	}
	name, err := checkToken(ctx, opts.Config)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "token is valid, this project will be agent %q\n", name)
	if insecureHub(opts.URL) {
		fmt.Fprintln(out, "warning: this hub uses plain http, anyone on the network path can read your token and messages. Put it behind https.")
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find own path: %w", err)
	}
	exe = filepath.ToSlash(exe)

	settings := filepath.Join(opts.Dir, ".claude", "settings.local.json")
	if err := updateSettings(settings, opts, exe); err != nil {
		return err
	}
	fmt.Fprintf(out, "updated %s\n", settings)
	warnIfNotIgnored(ctx, opts.Dir, settings, out)

	if err := registerServer(ctx, opts, exe, out); err != nil {
		return err
	}

	fmt.Fprintln(out, "\ndone, start your session in this folder with:")
	if opts.Mode == ModeChannel {
		fmt.Fprintf(out, "  claude --dangerously-load-development-channels server:%s\n", serverName)
	} else {
		fmt.Fprintln(out, "  claude")
	}
	return nil
}

func insecureHub(url string) bool {
	if !strings.HasPrefix(url, "http://") {
		return false
	}
	host := strings.TrimPrefix(url, "http://")
	return !strings.HasPrefix(host, "localhost") && !strings.HasPrefix(host, "127.0.0.1") && !strings.HasPrefix(host, "[::1]")
}

func checkToken(ctx context.Context, cfg Config) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL+"/api/agent/me", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach the hub at %s: %w", cfg.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errors.New("the hub rejected this token, create a new one in the web UI")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the hub answered with status %d, is %s really the hub?", resp.StatusCode, cfg.URL)
	}
	var me struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil || me.Name == "" {
		return "", fmt.Errorf("unexpected answer from %s, is that really the hub?", cfg.URL)
	}
	return me.Name, nil
}

// registerServer adds the bridge at local scope, which is private to this user and
// this project. Project scope would write the token into a file meant for sharing.
func registerServer(ctx context.Context, opts SetupOptions, exe string, out io.Writer) error {
	remove := exec.CommandContext(ctx, "claude", "mcp", "remove", "-s", "local", serverName)
	remove.Dir = opts.Dir
	remove.Run() // fails when nothing is registered yet, which is fine

	args := []string{
		"mcp", "add", "-s", "local", "--transport", "stdio",
		"-e", "AGENTIC_CHAT_URL=" + opts.URL,
		"-e", "AGENTIC_CHAT_TOKEN=" + opts.Token,
		serverName, "--", exe, "connect",
	}
	add := exec.CommandContext(ctx, "claude", args...)
	add.Dir = opts.Dir
	var output bytes.Buffer
	add.Stdout = &output
	add.Stderr = &output
	if err := add.Run(); err != nil {
		return fmt.Errorf("register the server with claude mcp add: %w\n%s", err, output.String())
	}
	fmt.Fprintln(out, "registered the mcp server for this project")
	return nil
}

// updateSettings merges into the existing file and leaves everything else in it alone.
func updateSettings(path string, opts SetupOptions, exe string) error {
	settings := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("%s is not valid JSON, fix it first: %w", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}

	// Without this an unattended session stalls on a permission prompt when it replies.
	permissions, _ := settings["permissions"].(map[string]any)
	if permissions == nil {
		permissions = map[string]any{}
	}
	allow, _ := permissions["allow"].([]any)
	rule := "mcp__" + serverName
	if !slices.Contains(allow, any(rule)) {
		allow = append(allow, rule)
	}
	permissions["allow"] = allow
	settings["permissions"] = permissions

	removeOwnHooks(settings)
	if opts.Mode == ModeHook {
		env, _ := settings["env"].(map[string]any)
		if env == nil {
			env = map[string]any{}
		}
		env["AGENTIC_CHAT_URL"] = opts.URL
		env["AGENTIC_CHAT_TOKEN"] = opts.Token
		settings["env"] = env
		addOwnHooks(settings, exe)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

func addOwnHooks(settings map[string]any, exe string) {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	quoted := `"` + exe + `"`
	watch := map[string]any{
		"type":        "command",
		"command":     quoted + " hook --watch",
		"asyncRewake": true,
		"timeout":     watchHookTimeout,
	}
	add := func(event string, hook map[string]any) {
		list, _ := hooks[event].([]any)
		hooks[event] = append(list, map[string]any{"hooks": []any{hook}})
	}
	add("SessionStart", watch)
	add("Stop", watch)
	add("UserPromptSubmit", map[string]any{"type": "command", "command": quoted + " hook"})
	settings["hooks"] = hooks
}

// removeOwnHooks takes out entries from an earlier setup, so running setup again
// or switching modes does not stack them up.
func removeOwnHooks(settings map[string]any) {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return
	}
	for event, raw := range hooks {
		groups, _ := raw.([]any)
		kept := []any{}
		for _, g := range groups {
			group, _ := g.(map[string]any)
			entries, _ := group["hooks"].([]any)
			isOurs := false
			for _, e := range entries {
				entry, _ := e.(map[string]any)
				command, _ := entry["command"].(string)
				if strings.Contains(command, "agentic-chat") && strings.Contains(command, " hook") {
					isOurs = true
				}
			}
			if !isOurs {
				kept = append(kept, g)
			}
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
}

// warnIfNotIgnored checks the file holding the token will not end up in a commit.
func warnIfNotIgnored(ctx context.Context, dir, path string, out io.Writer) {
	check := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	if check.Run() != nil {
		return
	}
	if exec.CommandContext(ctx, "git", "-C", dir, "check-ignore", "-q", path).Run() != nil {
		fmt.Fprintf(out, "warning: %s is not ignored by git and can hold your token, add it to .gitignore\n", path)
	}
}
