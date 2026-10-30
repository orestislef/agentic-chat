//go:build !clientonly

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/orestislef/agentic-chat/internal/auth"
	"github.com/orestislef/agentic-chat/internal/server"
	"github.com/orestislef/agentic-chat/internal/store"
)

func openStore(cfg server.Config) (*store.Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return store.Open(cfg.DatabasePath())
}

func runServe(ctx context.Context) error {
	cfg, err := server.ConfigFromEnv()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	s, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	return server.New(cfg, s, log).Run(ctx)
}

func runAdmin(ctx context.Context, args []string) error {
	if len(args) != 2 || (args[0] != "reset-password" && args[0] != "backup") {
		return fmt.Errorf("usage: agentic-chat admin reset-password <username> | backup <file>")
	}
	cfg, err := server.ConfigFromEnv()
	if err != nil {
		return err
	}
	s, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	if args[0] == "backup" {
		if err := s.Backup(ctx, args[1]); err != nil {
			return err
		}
		fmt.Println("wrote", args[1])
		return nil
	}
	u, err := s.UserByUsername(ctx, strings.ToLower(args[1]))
	if err != nil {
		return fmt.Errorf("user %q: %w", args[1], err)
	}
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	password := base64.RawURLEncoding.EncodeToString(buf)
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if err := s.SetPassword(ctx, u.ID, hash, ""); err != nil {
		return err
	}
	fmt.Printf("new password for %s: %s\nall their sessions were logged out\n", u.Username, password)
	return nil
}

func runHealthcheck() error {
	cfg, err := server.ConfigFromEnv()
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return err
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
