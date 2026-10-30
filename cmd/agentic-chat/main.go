// Command agentic-chat is both the hub and the client that runs next to a coding session.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/orestislef/agentic-chat/internal/client"
	"github.com/orestislef/agentic-chat/internal/version"
)

const usage = `agentic-chat lets separate coding sessions talk to each other.

On the server:
  agentic-chat serve                        run the hub
  agentic-chat admin reset-password <user>  set a new random password
  agentic-chat admin backup <file>          write a consistent copy of the database
  agentic-chat healthcheck                  exit 0 if the local hub answers

Next to a coding session:
  agentic-chat setup --url URL --token TOKEN [--mode channel|hook]
                                            connect the project in this folder
  agentic-chat connect                      stdio bridge, started by the session
  agentic-chat hook [--watch]               hook command, started by the session

  agentic-chat version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var code int
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(ctx)
	case "connect":
		err = runConnect(ctx, os.Args[2:])
	case "hook":
		code, err = runHook(ctx, os.Args[2:])
	case "setup":
		err = runSetup(ctx, os.Args[2:])
	case "admin":
		err = runAdmin(ctx, os.Args[2:])
	case "healthcheck":
		err = runHealthcheck()
	case "version", "--version", "-v":
		fmt.Println(version.Version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		code = 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		code = 1
	}
	stop()
	os.Exit(code)
}

// clientFlags adds the options every client command shares. Environment variables
// come first in the lookup, so a token does not have to show up in a process list.
func clientFlags(fs *flag.FlagSet) *client.Config {
	cfg := &client.Config{}
	fs.StringVar(&cfg.URL, "url", os.Getenv("AGENTIC_CHAT_URL"), "hub address")
	fs.StringVar(&cfg.Token, "token", os.Getenv("AGENTIC_CHAT_TOKEN"), "agent token")
	return cfg
}

func runConnect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	cfg := clientFlags(fs)
	noPush := fs.Bool("no-push", false, "do not push messages into the session, the hook commands deliver them")
	fs.Parse(args)
	if err := cfg.Validate(); err != nil {
		return err
	}
	// Stdout belongs to the protocol, logs can only go to stderr.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	bridge := client.NewBridge(*cfg, os.Stdin, os.Stdout, log)
	bridge.NoPush = *noPush
	return bridge.Run(ctx)
}

func runHook(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	cfg := clientFlags(fs)
	watch := fs.Bool("watch", false, "wait in the background and wake the session when a message arrives")
	fs.Parse(args)
	if err := cfg.Validate(); err != nil {
		// A hook that fails loudly would disturb every prompt of the session.
		fmt.Fprintln(os.Stderr, "agentic-chat:", err)
		return 0, nil
	}
	if *watch {
		return client.RunWatch(ctx, *cfg, os.Stderr), nil
	}
	return client.RunHook(ctx, *cfg, os.Stdin, os.Stdout, os.Stderr), nil
}

func runSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	cfg := clientFlags(fs)
	mode := fs.String("mode", client.ModeChannel, "how messages reach the session: channel or hook")
	dir := fs.String("dir", ".", "project folder")
	fs.Parse(args)
	if err := cfg.Validate(); err != nil {
		return err
	}
	return client.Setup(ctx, client.SetupOptions{Config: *cfg, Mode: *mode, Dir: *dir}, os.Stdout)
}
