// browserd — Browser Control MCP server (see PLAN.md).
//
// Usage:
//
//	browserd                 run the MCP server on stdio (for Claude Desktop/Code)
//	browserd setup           print port/token + extension install instructions
//	browserd --verbose       additionally dump every relayed CDP command
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"browserd/internal/audit"
	"browserd/internal/bridge"
	"browserd/internal/browser"
	"browserd/internal/config"
	"browserd/internal/server"
)

func main() {
	verbose := flag.Bool("verbose", false, "debug logging incl. every relayed CDP command/response")
	flag.Parse()

	// Stdout is the MCP transport; all logging goes to stderr.
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if err := run(log, *verbose, flag.Arg(0)); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, verbose bool, subcommand string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}

	// Pick + persist a stable port on first run: the extension options store
	// the port, so it must survive restarts.
	if cfg.Port == 0 {
		br := bridge.New(cfg.Token, nil, false, log)
		port, err := br.Start(0)
		if err != nil {
			return err
		}
		br.Close()
		cfg.Port = port
		if err := cfg.Save(); err != nil {
			return err
		}
	}

	if subcommand == "setup" {
		return printSetup(cfg, dir)
	}
	if subcommand != "" {
		return fmt.Errorf("unknown subcommand %q (try: browserd setup)", subcommand)
	}

	aud, err := audit.Open(dir)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer aud.Close()

	br := bridge.New(cfg.Token, cfg.PinnedOrigins, verbose, log)
	mgr := browser.NewManager(br, log)
	br.SetEventHandler(mgr.HandleEvent)
	br.SetConnHook(func(profile string, connected bool) {
		aud.Write(audit.Entry{Kind: "connect", Profile: profile, Decision: fmt.Sprintf("connected=%v", connected)})
		if !connected {
			mgr.DropProfile(profile)
		}
	})

	if _, err := br.Start(cfg.Port); err != nil {
		return fmt.Errorf("%w — is another browserd running?", err)
	}
	defer br.Close()
	log.Info("bridge listening", "addr", fmt.Sprintf("127.0.0.1:%d", cfg.Port), "stateDir", dir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mgr.StartIdleDetacher(ctx)

	srv := server.New(cfg, dir, br, mgr, aud, log)
	err = srv.Run(ctx)

	// Kill-switch path: on shutdown (Ctrl-C or client disconnect) detach all
	// debugger sessions so Chrome's banner clears immediately.
	dctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	br.DetachAll(dctx)
	cancel()
	aud.Write(audit.Entry{Kind: "kill", Decision: "shutdown"})

	if err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func printSetup(cfg *config.Config, dir string) error {
	fmt.Printf(`browserd setup
==============

State dir : %s
Port      : %d
Token     : %s

1. Build/locate the extension folder (extension/ with dist/ built).
2. In each Chrome profile you want Claude to reach:
     chrome://extensions -> enable "Developer mode" -> "Load unpacked"
     -> select the extension folder.
3. Click "Details" -> "Extension options" on the browserd bridge extension:
     - port  : %d
     - token : (paste the token above)
     - label : a name for this profile (personal, work, …)
4. Add browserd to your MCP client, e.g. Claude Code:
     claude mcp add browserd -- %s
5. Optional: pin the extension origin. After the first connection the
   extension id appears in the server log; add it to %s:
     pinned_origins = ["chrome-extension://<id>"]

The toolbar icon is the kill switch: one click severs the connection.
`, dir, cfg.Port, cfg.Token, cfg.Port, executablePath(), dir+"/config.toml")
	return nil
}

func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return "browserd"
	}
	return exe
}
