// Command bridge is the entrypoint for the Readest → BookOrbit sync bridge.
//
// Modes:
//
//	bridge --config bridge.yaml            run as a daemon (default)
//	bridge --config bridge.yaml --once     run a single sync pass and exit
//	bridge --daemon --config bridge.yaml   explicit daemon mode
//
// In this foundation phase the CLI loads and validates configuration, builds
// the logger, opens the state store, and wires the (stub) clients and engine,
// but performs no network sync. Running a sync pass reports that the engine is
// not yet implemented, which is the expected behavior until Phase 6.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/user/bookorbit-readest-sync/internal/bookorbit"
	"github.com/user/bookorbit-readest-sync/internal/config"
	"github.com/user/bookorbit-readest-sync/internal/logger"
	"github.com/user/bookorbit-readest-sync/internal/readest"
	"github.com/user/bookorbit-readest-sync/internal/sync"
	"github.com/user/bookorbit-readest-sync/internal/sync/state"
	"github.com/user/bookorbit-readest-sync/internal/util/httpclient"
)

// version is the build version, overridable at link time via -ldflags.
var version = "0.1.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Errors are reported here so run() stays testable.
		fmt.Fprintf(os.Stderr, "bridge: %v\n", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	var (
		configPath  = fs.String("config", "configs/bridge.yaml", "path to the config file")
		once        = fs.Bool("once", false, "run a single sync pass and exit")
		daemon      = fs.Bool("daemon", false, "run continuously on the poll interval (default)")
		showVersion = fs.Bool("version", false, "print version and exit")
	)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	// Load configuration. A missing file is tolerated (defaults + env apply);
	// a malformed file or invalid merged config is fatal.
	cfg, err := config.Load(*configPath)
	if err != nil {
		if errors.Is(err, config.ErrNoConfigFile) {
			// Non-fatal: note it on the logger once it exists.
			fmt.Fprintf(os.Stderr, "bridge: warning: %v; using defaults and environment\n", err)
		} else {
			return err
		}
	}

	log := logger.New(os.Stderr, cfg.Bridge.LogLevel, cfg.Bridge.LogFormat)

	// Open the persistent state store. Loading a missing file is fine.
	st := state.NewFileStore(cfg.Bridge.StateFile)
	if err := st.Load(); err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// Wire the (stub) clients and engine. None of these perform network I/O in
	// the foundation phase.
	deviceID := resolveDeviceID(cfg, st)
	httpDoer := httpclient.New(cfg.Bridge.HTTPTimeout)
	rdAuth := readest.NewAuth(cfg.Readest.SupabaseURL, cfg.Readest.SupabaseAnonKey, cfg.Readest.Email, cfg.Readest.Password, st, httpDoer, log)
	rdClient := readest.NewClient(cfg.Readest.SyncBaseURL, rdAuth, httpDoer, log, cfg.Bridge.HTTPTimeout)

	device := bookorbit.DeviceInfo{
		DeviceID:      deviceID,
		DeviceModel:   cfg.BookOrbit.DeviceName,
		PluginVersion: version,
	}
	boClient := bookorbit.NewClient(cfg.BookOrbit.ServerURL, cfg.BookOrbit.Username, cfg.AuthKey(), device, httpDoer, log, cfg.Bridge.MaxBodyBytes, cfg.Bridge.HTTPTimeout)

	engine := sync.NewEngine(cfg, rdAuth, rdClient, boClient, st, log)

	log.Info("bridge starting",
		"version", version,
		"mode", modeName(*once, *daemon),
		"state_file", cfg.Bridge.StateFile,
		"device_id", deviceID,
		"poll_interval", cfg.Bridge.PollInterval.String(),
	)

	// Set up graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Persist state (including any freshly generated device ID) on exit.
	defer func() {
		if err := st.Save(); err != nil {
			log.Error("failed to save state", "error", err)
		}
	}()

	if *once {
		runErr := engine.RunOnce(ctx)
		if errors.Is(runErr, sync.ErrNotImplemented) {
			log.Info("sync engine not implemented yet (foundation phase); nothing to do")
			return nil
		}
		return runErr
	}

	// Daemon mode (default).
	runErr := engine.Run(ctx)
	if errors.Is(runErr, sync.ErrNotImplemented) {
		log.Info("sync engine not implemented yet (foundation phase); nothing to do")
		return nil
	}
	if errors.Is(runErr, context.Canceled) {
		log.Info("shutdown requested; exiting")
		return nil
	}
	return runErr
}

// modeName describes the effective run mode for logging.
func modeName(once, daemon bool) string {
	if once {
		return "once"
	}
	return "daemon"
}
