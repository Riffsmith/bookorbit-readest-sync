// Command bridge is the entrypoint for the Readest → BookOrbit sync bridge.
//
// Modes:
//
//	bridge --config bridge.yaml            run as a daemon (default)
//	bridge --config bridge.yaml --once     run a single sync pass and exit
//	bridge --daemon --config bridge.yaml   explicit daemon mode
//
// The CLI loads and validates configuration, builds the logger, opens the
// state store, wires the Readest/BookOrbit clients and sync engine, and then
// runs a single sync pass (--once) or a continuous poll loop (default).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/bookorbit"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/config"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/logger"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/readest"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/sync"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/sync/state"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/util"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/util/httpclient"
)

// version is the build version, overridable at link time via -ldflags.
var version = "0.1.0-dev"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		// Errors are reported here so run() stays testable.
		fmt.Fprintf(os.Stderr, "bridge: %v\n", err)
		os.Exit(1)
	}
}

// run executes the CLI end to end and returns the first fatal error, or nil
// on success — which includes a graceful signal-driven shutdown, --version,
// and -h/--help. stdout/stderr are injected so tests can capture CLI output
// without touching the process's real streams; main() always calls this with
// the real os.Stdout/os.Stderr, so production behavior is unchanged.
func run(argv []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath  = fs.String("config", "configs/bridge.yaml", "path to the config file")
		once        = fs.Bool("once", false, "run a single sync pass and exit")
		daemon      = fs.Bool("daemon", false, "run continuously on the poll interval (default)")
		showVersion = fs.Bool("version", false, "print version and exit")
	)
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h/--help already printed usage via fs.Usage(); by Unix
			// convention this is a successful invocation, not a failure.
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return nil
	}

	// Load configuration. A missing file is tolerated (defaults + env apply);
	// a malformed file or invalid merged config is fatal.
	cfg, err := config.Load(*configPath)
	if err != nil {
		if errors.Is(err, config.ErrNoConfigFile) {
			// Non-fatal: note it on the logger once it exists.
			fmt.Fprintf(stderr, "bridge: warning: %v; using defaults and environment\n", err)
		} else {
			return err
		}
	}

	log := logger.New(stderr, cfg.Bridge.LogLevel, cfg.Bridge.LogFormat)

	// Security-hardening transport warning. Fires once at startup, only when
	// the operator has explicitly set bookorbit.allow_insecure_transport=true,
	// because the only way the boolean can matter (cleartext http to a
	// non-loopback host) is a configuration Validate has already accepted
	// by virtue of the opt-in. The x-auth-key header is the unsalted MD5 of
	// the password (a password-equivalent credential); naming the host here
	// documents the operator's affirmative decision in the daemon log so the
	// cleartext transmission is not silent. See
	// docs/security-hardening-bookorbit-url-validation-design.md §3 Decision I.
	if cfg.BookOrbit.AllowInsecureTransport {
		log.Warn("bridge: bookorbit.server_url uses cleartext http to a non-loopback host; x-auth-key (MD5 of password) will travel in cleartext",
			"url", cfg.BookOrbit.ServerURL,
			"host", util.HostOf(cfg.BookOrbit.ServerURL))
	}

	// Open the persistent state store. Loading a missing file is fine.
	st := state.NewFileStore(cfg.Bridge.StateFile)
	if err := st.Load(); err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// Wire the Readest/BookOrbit clients and engine. None of these perform
	// network I/O on construction; the first network call happens inside
	// engine.RunOnce/Run.
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

	// One-time BookOrbit connectivity probe. This is never fatal: a bad
	// BookOrbit credential or an unreachable server is already handled by
	// the engine's own retry/backoff and "log and continue" policy on every
	// scheduled pass (internal/sync/engine.go). The probe exists only to
	// give an operator immediate, visible feedback on the most common
	// first-run mistake (wrong bookorbit.username/password/server_url)
	// instead of discovering it only after poll_interval has elapsed.
	if err := boClient.Auth(ctx); err != nil {
		log.Warn("bookorbit connectivity check failed; sync will retry on schedule", "error", err)
	} else {
		log.Info("bookorbit connectivity check passed")
	}

	if *once {
		return engine.RunOnce(ctx)
	}

	// Daemon mode (default). --daemon is accepted purely so an explicit
	// invocation (e.g. a systemd ExecStart line) can self-document intent;
	// it has no effect on control flow since daemon is already the default.
	runErr := engine.Run(ctx)
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
