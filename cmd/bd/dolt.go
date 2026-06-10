package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/doltutil"
	"github.com/steveyegge/beads/internal/ui"
	"golang.org/x/term"
)

var doltCmd = &cobra.Command{
	Use:     "dolt",
	GroupID: "setup",
	Short:   "Configure Dolt database settings",
	Long: `Configure and manage Dolt database settings and server lifecycle.

Beads uses a dolt sql-server for all database operations. The server is
auto-started transparently when needed. Use these commands for explicit
control or diagnostics.

Server lifecycle:
  bd dolt start        Start the Dolt server for this project
  bd dolt stop         Stop the Dolt server for this project
  bd dolt status       Show Dolt server status

Configuration:
  bd dolt show         Show current Dolt configuration with connection test
  bd dolt set <k> <v>  Set a configuration value
  bd dolt test         Test server connection

Version control:
  bd dolt commit       Commit pending changes
  bd dolt push         Push commits to Dolt remote
  bd dolt pull         Pull commits from Dolt remote

Remote management:
  bd dolt remote add <name> <url>   Add a Dolt remote
  bd dolt remote list                List configured remotes
  bd dolt remote remove <name>       Remove a Dolt remote

Configuration keys for 'bd dolt set':
  database  Database name (default: issue prefix or "beads")
  host      Server host (default: 127.0.0.1)
  port      Server port (auto-detected; override with bd dolt set port <N>)
  user      MySQL user (default: root)
  data-dir  Custom dolt data directory (absolute path; default: .beads/dolt)

Flags for 'bd dolt set':
  --update-config  Also write to config.yaml for team-wide defaults

Examples:
  bd dolt set database myproject
  bd dolt set host 192.168.1.100 --update-config
  bd dolt set data-dir /home/user/.beads-dolt/myproject
  bd dolt test`,
}

var doltShowCmd = &cobra.Command{
	Use:           "show",
	Short:         "Show current Dolt configuration with connection status",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-show")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		return showDoltConfig(true)
	},
}

var doltSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a Dolt configuration value",
	Long: `Set a Dolt configuration value in metadata.json.

Keys:
  database  Database name (default: issue prefix or "beads")
  host      Server host (default: 127.0.0.1)
  port      Server port (auto-detected; override with bd dolt set port <N>)
  user      MySQL user (default: root)
  data-dir  Custom dolt data directory (absolute path; default: .beads/dolt)

Use --update-config to also write to config.yaml for team-wide defaults.

Examples:
  bd dolt set database myproject
  bd dolt set host 192.168.1.100
  bd dolt set port 3307 --update-config
  bd dolt set data-dir /home/user/.beads-dolt/myproject`,
	Args:          cobra.ExactArgs(2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-set")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if !usesSQLServer() {
			fmt.Fprintln(os.Stderr, "Error: 'bd dolt set' is not supported in embedded mode (no Dolt server)")
			return SilentExit()
		}
		key := args[0]
		value := args[1]
		updateConfig, _ := cmd.Flags().GetBool("update-config")
		return setDoltConfig(key, value, updateConfig)
	},
}

var doltTestCmd = &cobra.Command{
	Use:   "test",
	Short: "Test connection to Dolt server",
	Long: `Test the connection to the configured Dolt server.

This verifies that:
  1. The server is reachable at the configured host:port
  2. The connection can be established

Use this before switching to server mode to ensure the server is running.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-test")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if !usesSQLServer() {
			fmt.Fprintln(os.Stderr, "Error: 'bd dolt test' is not supported in embedded mode (no Dolt server)")
			return SilentExit()
		}
		return testDoltConnection()
	},
}

// isRemoteNotFoundErr checks whether the error is a Dolt "remote not found"
// error. This typically happens when the remote was added via `dolt remote add`
// (filesystem config) but not via `bd dolt remote add` (which also registers it
// in the SQL server's dolt_remotes table).
func isRemoteNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "remote") && strings.Contains(msg, "not found")
}

// isDivergedHistoryErr checks whether the error indicates that local and remote
// Dolt histories have diverged. This happens when independent pushes create
// separate commit histories with no common merge base (e.g., two agents
// bootstrapping from scratch and pushing to the same remote, or a local
// database being re-initialized while the remote retains the old history).
func isDivergedHistoryErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no common ancestor") ||
		strings.Contains(msg, "can't find common ancestor") ||
		strings.Contains(msg, "cannot find common ancestor")
}

// printNoRemoteGuidance prints an informational message (to stdout) when
// push or pull is attempted but no Dolt remote is configured. Exits 0 because
// the absence of a remote is a valid configuration — not an error.
func printNoRemoteGuidance() {
	fmt.Println("No remote is configured — skipping.")
	fmt.Println("")
	fmt.Println("For solo use, pushing is optional — your issues are stored locally")
	fmt.Println("in .beads/ and versioned by Dolt automatically.")
	fmt.Println("")
	fmt.Println("To set up remote sync (for backup or team sharing):")
	fmt.Println("  bd dolt remote add origin <url>")
	fmt.Println("  bd dolt push")
	fmt.Println("")
	fmt.Println("Supported remote URLs:")
	fmt.Println("  • GitHub (via git):   git+ssh://git@github.com/org/repo.git")
	fmt.Println("  • DoltHub:            https://doltremoteapi.dolthub.com/org/repo")
	fmt.Println("  • Azure Blob Storage: az://account.blob.core.windows.net/container/path")
}

func adoptGitOriginRemoteForPush(ctx context.Context, st storage.DoltStorage) (bool, error) {
	remotes, err := st.ListRemotes(ctx)
	if err != nil {
		return false, err
	}
	if len(remotes) > 0 {
		return false, nil
	}
	beadsDir := selectedDoltBeadsDir()
	if beadsDir == "" {
		return false, fmt.Errorf("no active beads workspace")
	}
	originURL, err := gitOriginGetURLForActiveRepo(ctx)
	if err != nil || originURL == "" {
		return false, nil
	}
	remoteURL := normalizeRemoteURL(originURL)
	if err := st.AddRemote(ctx, "origin", remoteURL); err != nil {
		return false, err
	}

	if err := config.SetYamlConfigInDir(beadsDir, "sync.remote", remoteURL); err != nil {
		return false, fmt.Errorf("failed to persist sync.remote to config.yaml: %w", err)
	}
	commitBeadsConfigForActiveRepo(ctx, "bd: update sync.remote")
	return true, nil
}

// printDivergedHistoryGuidance prints recovery guidance when push/pull fails
// due to diverged local and remote histories.
func printDivergedHistoryGuidance(operation string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Local and remote Dolt histories have diverged.")
	fmt.Fprintln(os.Stderr, "This means the local database and the remote have independent commit")
	fmt.Fprintln(os.Stderr, "histories with no common merge base.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Recovery options:")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  1. Keep remote, discard local (recommended if remote is authoritative):")
	fmt.Fprintln(os.Stderr, "       bd bootstrap              # re-clone from remote")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  2. Keep local, overwrite remote (if local is authoritative):")
	fmt.Fprintln(os.Stderr, "       bd dolt push --force       # force-push local history to remote")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  3. Manual recovery (re-initialize local database):")
	fmt.Fprintln(os.Stderr, "       rm -rf .beads/dolt         # delete local Dolt database")
	fmt.Fprintln(os.Stderr, "       bd bootstrap              # re-clone from remote")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Tip: This usually happens when multiple agents independently initialize")
	fmt.Fprintln(os.Stderr, "databases and push to the same remote. Use 'bd bootstrap' to clone an")
	fmt.Fprintln(os.Stderr, "existing remote instead of 'bd init' to avoid divergent histories.")
}

var doltPushCmd = &cobra.Command{
	Use:   "push",
	Short: "Push commits to Dolt remote",
	Long: `Push local Dolt commits to the configured remote.

Requires a Dolt remote to be configured in the database directory.
For Hosted Dolt, set DOLT_REMOTE_USER and DOLT_REMOTE_PASSWORD environment
variables for authentication.

Use --force to overwrite remote changes (e.g., when the remote has
uncommitted changes in its working set).

Use --remote to push to a specific named remote instead of the default.
The remote must already exist (see 'bd dolt remote add').`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-push")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		ctx := context.Background()
		st := getStore()
		if st == nil {
			fmt.Fprintf(os.Stderr, "Error: no store available\n")
			return SilentExit()
		}
		force, _ := cmd.Flags().GetBool("force")
		remote, _ := cmd.Flags().GetString("remote")
		if remote != "" {
			fmt.Printf("Pushing to Dolt remote %q...\n", remote)
			if err := st.PushRemote(ctx, remote, force); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				if isRemoteNotFoundErr(err) {
					fmt.Fprintf(os.Stderr, "\nRemote %q is not configured.\n", remote)
					fmt.Fprintln(os.Stderr, "Use 'bd dolt remote add <name> <url>' to add it.")
					fmt.Fprintln(os.Stderr, "Use 'bd dolt remote list' to see configured remotes.")
				} else if isDivergedHistoryErr(err) {
					printDivergedHistoryGuidance("push --force")
				}
				return SilentExit()
			}
			fmt.Println("Push complete.")
			return nil
		}
		if adopted, err := adoptGitOriginRemoteForPush(ctx, st); err != nil {
			fmt.Fprintf(os.Stderr, "Error: failed to adopt git origin as Dolt remote: %v\n", err)
			return SilentExit()
		} else if adopted {
			fmt.Println("Configured Dolt remote origin from git origin.")
		}
		fmt.Println("Pushing to Dolt remote...")

		var pushErr error
		if force {
			pushErr = st.ForcePush(ctx)
		} else {
			pushErr = st.Push(ctx)
		}
		if pushErr != nil {
			if isRemoteNotFoundErr(pushErr) {
				printNoRemoteGuidance()
				return nil
			}
			fmt.Fprintf(os.Stderr, "Error: %v\n", pushErr)
			if isDivergedHistoryErr(pushErr) {
				op := "push"
				if force {
					op = "push --force"
				}
				printDivergedHistoryGuidance(op)
			}
			return SilentExit()
		}
		fmt.Println("Push complete.")
		return nil
	},
}

var doltPullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Pull commits from Dolt remote",
	Long: `Pull commits from the configured Dolt remote into the local database.

Requires a Dolt remote to be configured in the database directory.
For Hosted Dolt, set DOLT_REMOTE_USER and DOLT_REMOTE_PASSWORD environment
variables for authentication.

Use --remote to pull from a specific named remote instead of the default.
The remote must already exist (see 'bd dolt remote add').`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-pull")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		ctx := context.Background()
		st := getStore()
		if st == nil {
			fmt.Fprintf(os.Stderr, "Error: no store available\n")
			return SilentExit()
		}
		remote, _ := cmd.Flags().GetString("remote")
		if remote != "" {
			fmt.Printf("Pulling from Dolt remote %q...\n", remote)
			if err := st.PullRemote(ctx, remote); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				if isRemoteNotFoundErr(err) {
					fmt.Fprintf(os.Stderr, "\nRemote %q is not configured.\n", remote)
					fmt.Fprintln(os.Stderr, "Use 'bd dolt remote add <name> <url>' to add it.")
					fmt.Fprintln(os.Stderr, "Use 'bd dolt remote list' to see configured remotes.")
				} else if isDivergedHistoryErr(err) {
					printDivergedHistoryGuidance("pull")
				}
				return SilentExit()
			}
			fmt.Println("Pull complete.")
			return nil
		}
		fmt.Println("Pulling from Dolt remote...")
		if err := st.Pull(ctx); err != nil {
			if isRemoteNotFoundErr(err) {
				printNoRemoteGuidance()
				return nil
			}
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			if isDivergedHistoryErr(err) {
				printDivergedHistoryGuidance("pull")
			}
			return SilentExit()
		}
		fmt.Println("Pull complete.")
		return nil
	},
}

var doltCommitCmd = &cobra.Command{
	Use:   "commit",
	Short: "Create a Dolt commit from pending changes",
	Long: `Create a Dolt commit from any uncommitted changes in the working set.

This is the primary commit point for batch mode. When auto-commit is set to
"batch", changes accumulate in the working set across multiple bd commands and
are committed together here with a descriptive summary message.

Also useful before push operations that require a clean working set, or when
auto-commit was off or changes were made externally.

For more options (--stdin, custom messages), see: bd vc commit`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-commit")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		ctx := context.Background()
		st := getStore()
		if st == nil {
			fmt.Fprintf(os.Stderr, "Error: no store available\n")
			return SilentExit()
		}
		msg, _ := cmd.Flags().GetString("message")
		if msg == "" {
			msg = fmt.Sprintf("bd: dolt commit (auto-commit) by %s", getActor())
		}
		if err := st.Commit(ctx, msg); err != nil {
			if isDoltNothingToCommit(err) {
				fmt.Println("Nothing to commit.")
				return nil
			}
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return SilentExit()
		}
		commandDidExplicitDoltCommit = true
		fmt.Println("Committed.")
		return nil
	},
}

var doltStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Dolt SQL server for this project",
	Long: `Start a dolt sql-server for the current beads project.

The server runs in the background on a per-project port derived from the
project path. PID and logs are stored in .beads/.

The server auto-starts transparently when needed, so manual start is rarely
required. Use this command for explicit control or diagnostics.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-start")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if !usesSQLServer() {
			fmt.Fprintln(os.Stderr, "Error: 'bd dolt start' is not supported in embedded mode (no Dolt server)")
			return SilentExit()
		}
		beadsDir := selectedDoltBeadsDir()
		if beadsDir == "" {
			return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
		}
		serverDir := doltserver.ResolveServerDir(beadsDir)

		state, err := doltserver.Start(serverDir)
		if err != nil {
			if strings.Contains(err.Error(), "already running") {
				fmt.Println(err)
				return nil
			}
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return SilentExit()
		}

		fmt.Printf("Dolt server started (PID %d, port %d)\n", state.PID, state.Port)
		fmt.Printf("  Data: %s\n", state.DataDir)
		fmt.Printf("  Logs: %s\n", doltserver.LogPath(serverDir))
		if doltserver.IsSharedServerMode() {
			fmt.Println("  Mode: shared server")
		}
		if doltserver.IsDebugMode() {
			fmt.Println("  Debug: on (loglevel=debug, --prof cpu)")
			fmt.Printf("  Profile dir: %s\n", doltserver.DebugProfileDir(beadsDir))
			fmt.Println("  Note: cpu.pprof is written when the server exits cleanly (bd dolt stop).")
		}
		return nil
	},
}

var doltStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Dolt SQL server for this project",
	Long: `Stop the dolt sql-server managed by beads for the current project.

This sends a graceful shutdown signal. The server will restart automatically
on the next bd command unless auto-start is disabled.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-stop")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if !usesSQLServer() {
			fmt.Fprintln(os.Stderr, "Error: 'bd dolt stop' is not supported in embedded mode (no Dolt server)")
			return SilentExit()
		}
		beadsDir := selectedDoltBeadsDir()
		if beadsDir == "" {
			return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
		}
		serverDir := doltserver.ResolveServerDir(beadsDir)
		force, _ := cmd.Flags().GetBool("force")

		if err := doltserver.StopWithForce(serverDir, force); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return SilentExit()
		}
		fmt.Println("Dolt server stopped.")
		return nil
	},
}

var doltStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Dolt engine status",
	Long: `Show the status of the Dolt engine for the current project.

In embedded mode, reports that the Dolt engine runs in-process and shows
the on-disk data directory. For beads-managed (local) servers, displays
PID, port, and data directory from the local PID file. For externally-
managed servers — either a remote dolt_server_host or a local server
managed outside bd (dolt.auto-start: false, e.g. an orchestrator-shared
sql-server) — pings the configured endpoint via SQL and reports
reachability, server version, and database.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-status")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		beadsDir := selectedDoltBeadsDir()
		if beadsDir == "" {
			return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
		}
		if !usesSQLServer() {
			return showEmbeddedDoltStatus(beadsDir)
		}

		cfg, cfgErr := configfile.Load(beadsDir)
		if cfgErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: cannot load .beads config (%v); falling back to PID-file status path\n", cfgErr)
		}
		if cfg != nil && shouldUseExternalDoltStatus(cfg, doltserver.IsAutoStartDisabled()) {
			return runExternalDoltStatus(beadsDir, cfg)
		}

		serverDir := doltserver.ResolveServerDir(beadsDir)

		state, err := doltserver.IsRunning(serverDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return SilentExit()
		}
		return renderLocalDoltStatus(state, serverDir)
	},
}

// renderLocalDoltStatus writes the bd-managed (local PID-file) status of
// the Dolt server to stdout, honoring jsonOutput. Extracted from the
// doltStatusCmd Run closure so the bd-managed output path is unit-testable
// without requiring a live dolt sql-server (the externally-managed path
// is exercised by TestRunExternalDoltStatus_Unreachable).
func renderLocalDoltStatus(state *doltserver.State, serverDir string) error {
	if jsonOutput {
		return outputJSON(state)
	}
	if state == nil || !state.Running {
		cfg := doltserver.DefaultConfig(serverDir)
		fmt.Println("Dolt server: not running")
		fmt.Printf("  Expected port: %d\n", cfg.Port)
		return nil
	}
	fmt.Println("Dolt server: running")
	fmt.Printf("  PID:  %d\n", state.PID)
	fmt.Printf("  Port: %d\n", state.Port)
	fmt.Printf("  Data: %s\n", state.DataDir)
	fmt.Printf("  Logs: %s\n", doltserver.LogPath(serverDir))
	if doltserver.IsSharedServerMode() {
		fmt.Println("  Mode: shared server")
	}
	if doltserver.IsDebugMode() {
		fmt.Println("  Debug: on (loglevel=debug, --prof cpu)")
		fmt.Printf("  Profile dir: %s\n", doltserver.DebugProfileDir(serverDir))
	}
	return nil
}

// shouldUseExternalDoltStatus reports whether bd dolt status should treat
// the server as externally-managed and probe via SQL instead of consulting
// the local PID file. Returns true when:
//   - dolt_mode=server with a non-local host (Hosted Dolt, remote shared
//     sql-server) — the PID file is on a different machine.
//   - dolt_mode=server with a local host but bd auto-start is disabled —
//     the server lifecycle is owned by something outside bd (e.g. an
//     orchestrator or systemd unit), so no bd PID file exists. Without
//     this branch, status reports "not running" even when bd CRUD
//     commands successfully connect to the server (be-0eyj).
//
// When false, the caller falls back to the PID-file path that reports
// PID, port, log path, and data directory for bd-managed servers.
//
// autoStartDisabled is passed in (rather than read here) so the predicate
// is pure and unit-testable without manipulating package-level config.
func shouldUseExternalDoltStatus(cfg *configfile.Config, autoStartDisabled bool) bool {
	if cfg == nil || !cfg.IsDoltServerMode() {
		return false
	}
	if !isLocalHost(cfg.GetDoltServerHost()) {
		return true
	}
	return autoStartDisabled
}

// isLocalHost reports whether host refers to this machine. Used to
// distinguish beads-managed local servers from externally-hosted ones.
func isLocalHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return true // empty defaults to local
	}
	switch h {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

// runExternalDoltStatus queries an externally-hosted Dolt server and prints
// (or returns, for --json) status. Unlike the local path, there is no PID or
// log file — reachability, version, host/port/database, and TLS mode are the
// user-relevant signals.
func runExternalDoltStatus(beadsDir string, cfg *configfile.Config) error {
	host := cfg.GetDoltServerHost()
	port := doltserver.DefaultConfig(beadsDir).Port
	user := cfg.GetDoltServerUser()
	database := cfg.GetDoltDatabase()
	tls := cfg.GetDoltServerTLS()
	password := cfg.GetDoltServerPasswordForPort(port)

	dsn := doltutil.ServerDSN{
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
		TLS:      tls,
		Timeout:  5 * time.Second,
	}.String()

	result := map[string]interface{}{
		"mode":     "external",
		"host":     host,
		"port":     port,
		"user":     user,
		"database": database,
		"tls":      tls,
	}

	db, openErr := sql.Open("mysql", dsn)
	var running bool
	var version string
	var connErr error

	if openErr == nil {
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if pingErr := db.PingContext(ctx); pingErr != nil {
			connErr = pingErr
		} else {
			running = true
			// Best-effort version lookup; don't treat errors as fatal.
			_ = db.QueryRowContext(ctx, "SELECT @@version").Scan(&version)
		}
	} else {
		connErr = openErr
	}

	result["running"] = running
	if version != "" {
		result["version"] = version
	}
	if connErr != nil {
		result["error"] = connErr.Error()
	}

	if jsonOutput {
		return outputJSON(result)
	}

	if running {
		fmt.Println("Dolt server: running (external)")
	} else {
		fmt.Println("Dolt server: not reachable (external)")
	}
	fmt.Printf("  Host:     %s\n", host)
	fmt.Printf("  Port:     %d\n", port)
	fmt.Printf("  Database: %s\n", database)
	fmt.Printf("  User:     %s\n", user)
	fmt.Printf("  TLS:      %t\n", tls)
	if version != "" {
		fmt.Printf("  Version:  %s\n", version)
	}
	if connErr != nil {
		fmt.Printf("  Error:    %v\n", connErr)
	}
	return nil
}

// showEmbeddedDoltStatus reports Dolt engine status when running in
// embedded mode. There is no separate server process; the engine runs
// in-process and data lives at .beads/embeddeddolt/.
func showEmbeddedDoltStatus(beadsDir string) error {
	dataDir := filepath.Join(beadsDir, "embeddeddolt")
	dataDirExists := false
	if info, err := os.Stat(dataDir); err == nil && info.IsDir() {
		dataDirExists = true
	}

	if jsonOutput {
		return outputJSON(map[string]interface{}{
			"mode":            "embedded",
			"server_running":  false,
			"data_dir":        dataDir,
			"data_dir_exists": dataDirExists,
		})
	}

	fmt.Println("Dolt engine: embedded (in-process, no server)")
	fmt.Printf("  Data: %s\n", dataDir)
	if !dataDirExists {
		fmt.Printf("  %s\n", ui.RenderWarn("Data directory does not exist — run 'bd init' to create it"))
	}
	return nil
}

var doltKillallCmd = &cobra.Command{
	Use:   "killall",
	Short: "Kill all orphan Dolt server processes",
	Long: `Find and kill orphan dolt sql-server processes not tracked by the
canonical PID file for the current repo's Dolt data directory.

Under an orchestrator, the canonical server lives at $GT_ROOT/.beads/. Any other
dolt sql-server processes using that shared data directory are considered
orphans and will be killed.

In standalone mode, only dolt sql-server processes using the current
project's Dolt data directory are eligible for cleanup. Other projects'
servers are preserved.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-killall")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if !usesSQLServer() {
			fmt.Fprintln(os.Stderr, "Error: 'bd dolt killall' is not supported in embedded mode (no Dolt server)")
			return SilentExit()
		}
		beadsDir := selectedDoltBeadsDir()
		if beadsDir == "" {
			beadsDir = "."
		}

		killed, err := doltserver.KillStaleServers(beadsDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return SilentExit()
		}

		if len(killed) == 0 {
			fmt.Println("No orphan dolt servers found.")
		} else {
			fmt.Printf("Killed %d orphan dolt server(s): %v\n", len(killed), killed)
		}
		return nil
	},
}

// staleDatabasePrefixes identifies test/agent databases that should not persist
// on the production Dolt server. These accumulate from interrupted test runs and
// terminated agents, wasting server memory.
// - testdb_*: BEADS_TEST_MODE=1 FNV hash of temp paths
// - doctest_*: doctor test helpers
// - doctortest_*: doctor test helpers
// - beads_pt*: orchestrator patrol_helpers_test.go random prefixes
// - beads_vr*: orchestrator mail/router_test.go random prefixes
// - beads_t[0-9a-f]*: protocol test random prefixes (t + 8 hex chars)
var staleDatabasePrefixes = []string{"testdb_", "doctest_", "doctortest_", "beads_pt", "beads_vr", "beads_t"}

var doltCleanDatabasesCmd = &cobra.Command{
	Use:   "clean-databases",
	Short: "Drop stale test databases from the Dolt server",
	Long: `Identify and drop leftover test and agent databases that accumulate
on the shared Dolt server from interrupted test runs and terminated agents.

Stale database prefixes: testdb_*, doctest_*, doctortest_*, beads_pt*, beads_vr*, beads_t*

These waste server memory and can degrade performance under concurrent load.
Use --dry-run to see what would be dropped without actually dropping.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-clean-databases")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if !usesSQLServer() {
			fmt.Fprintln(os.Stderr, "Error: 'bd dolt clean-databases' is not supported in embedded mode (no Dolt server)")
			return SilentExit()
		}
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		db, cleanup, err := openDoltServerConnection()
		if err != nil {
			return err
		}
		defer cleanup()

		listCtx, listCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer listCancel()

		rows, err := db.QueryContext(listCtx, "SHOW DATABASES")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing databases: %v\n", err)
			return SilentExit()
		}
		defer rows.Close()

		var stale []string
		for rows.Next() {
			var dbName string
			if err := rows.Scan(&dbName); err != nil {
				continue
			}
			for _, prefix := range staleDatabasePrefixes {
				if strings.HasPrefix(dbName, prefix) {
					stale = append(stale, dbName)
					break
				}
			}
		}

		if len(stale) == 0 {
			fmt.Println("No stale databases found.")
			return nil
		}

		fmt.Printf("Found %d stale databases:\n", len(stale))
		for _, name := range stale {
			fmt.Printf("  %s\n", name)
		}

		if dryRun {
			fmt.Println("\n(dry run — no databases dropped)")
			return nil
		}

		fmt.Println()
		dropped := 0
		failures := 0
		consecutiveTimeouts := 0
		const (
			batchSize         = 5 // Drop this many before pausing
			batchPause        = 2 * time.Second
			backoffPause      = 10 * time.Second
			timeoutThreshold  = 3 // Consecutive timeouts before backoff
			perDropTimeout    = 30 * time.Second
			maxConsecFailures = 10 // Stop after this many consecutive failures
		)

		for i, name := range stale {
			// Circuit breaker: back off when server is overwhelmed
			if consecutiveTimeouts >= timeoutThreshold {
				fmt.Fprintf(os.Stderr, "  ⚠ %d consecutive timeouts — backing off %s\n",
					consecutiveTimeouts, backoffPause)
				time.Sleep(backoffPause)
				consecutiveTimeouts = 0
			}

			// Stop if too many consecutive failures — server is likely unhealthy
			if failures >= maxConsecFailures {
				fmt.Fprintf(os.Stderr, "\n✗ Aborting: %d consecutive failures suggest server is unhealthy.\n", failures)
				fmt.Fprintf(os.Stderr, "  Dropped %d/%d before stopping.\n", dropped, len(stale))
				return SilentExit()
			}

			// Per-operation timeout: DROP DATABASE can be slow on Dolt
			dropCtx, dropCancel := context.WithTimeout(context.Background(), perDropTimeout)
			// Escape backticks in database name to prevent SQL injection (` → ``)
			safeName := strings.ReplaceAll(name, "`", "``")
			_, err := db.ExecContext(dropCtx, fmt.Sprintf("DROP DATABASE `%s`", safeName)) //nolint:gosec // G201: identifier-escaped
			dropCancel()
			if err != nil {
				fmt.Fprintf(os.Stderr, "  FAIL: %s: %v\n", name, err)
				failures++
				if isTimeoutError(err) {
					consecutiveTimeouts++
				}
			} else {
				fmt.Printf("  Dropped: %s\n", name)
				dropped++
				failures = 0
				consecutiveTimeouts = 0
			}

			// Rate limiting: pause between batches to let the server breathe
			if (i+1)%batchSize == 0 && i+1 < len(stale) {
				fmt.Printf("  [%d/%d] pausing %s...\n", i+1, len(stale), batchPause)
				time.Sleep(batchPause)
			}
		}
		fmt.Printf("\nDropped %d/%d stale databases.\n", dropped, len(stale))
		return nil
	},
}

// --- Dolt remote management commands ---

type doltRemoteAddStore interface {
	ListRemotes(ctx context.Context) ([]storage.RemoteInfo, error)
	AddRemote(ctx context.Context, name, url string) error
	RemoveRemote(ctx context.Context, name string) error
}

type doltRemoteAddResult struct {
	Canceled bool
}

type doltRemoteOverwriteConfirmer func(surface, name, existingURL, newURL string) bool

func confirmDoltRemoteOverwrite(surface, name, existingURL, newURL string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return true
	}
	fmt.Printf("  Remote %q already exists on %s: %s\n", name, surface, existingURL)
	fmt.Printf("  Overwrite with: %s\n", newURL)
	fmt.Print("  Overwrite? (y/N): ")
	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "y" || response == "yes"
}

func findDoltRemoteURL(remotes []storage.RemoteInfo, name string) string {
	for _, remote := range remotes {
		if remote.Name == name {
			return remote.URL
		}
	}
	return ""
}

func ensureDoltRemote(ctx context.Context, st doltRemoteAddStore, name, url string, confirm doltRemoteOverwriteConfirmer) (doltRemoteAddResult, error) {
	remotes, err := st.ListRemotes(ctx)
	if err != nil {
		return doltRemoteAddResult{}, fmt.Errorf("list existing remotes: %w", err)
	}

	existingURL := findDoltRemoteURL(remotes, name)
	if existingURL == "" {
		if err := st.AddRemote(ctx, name, url); err != nil {
			return doltRemoteAddResult{}, fmt.Errorf("add remote %s: %w", name, err)
		}
		return doltRemoteAddResult{}, nil
	}

	if doltutil.RemoteURLsMatch(existingURL, url) {
		return doltRemoteAddResult{}, nil
	}

	if !confirm("SQL server", name, existingURL, url) {
		return doltRemoteAddResult{Canceled: true}, nil
	}
	if err := st.RemoveRemote(ctx, name); err != nil {
		return doltRemoteAddResult{}, fmt.Errorf("remove existing remote %s: %w", name, err)
	}
	if err := st.AddRemote(ctx, name, url); err != nil {
		return doltRemoteAddResult{}, fmt.Errorf("add remote %s: %w", name, err)
	}
	return doltRemoteAddResult{}, nil
}

var doltRemoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Manage Dolt remotes",
	Long: `Manage Dolt remotes for push/pull replication.

Subcommands:
  add <name> <url>   Add a new remote
  list               List all configured remotes
  remove <name>      Remove a remote`,
}

var doltRemoteAddCmd = &cobra.Command{
	Use:           "add <name> <url>",
	Short:         "Add a Dolt remote",
	Args:          cobra.ExactArgs(2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-remote-add")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		ctx := context.Background()
		st := getStore()
		if st == nil {
			fmt.Fprintf(os.Stderr, "Error: no store available\n")
			return SilentExit()
		}
		name, url := args[0], args[1]

		result, err := ensureDoltRemote(ctx, st, name, url, confirmDoltRemoteOverwrite)
		if err != nil {
			if jsonOutput {
				return outputJSONError(err, "remote_add_failed")
			}
			fmt.Fprintf(os.Stderr, "Error adding remote: %v\n", err)
			return SilentExit()
		}
		if result.Canceled {
			fmt.Println("Canceled.")
			return nil
		}

		if name == "origin" {
			if err := config.SetYamlConfig("sync.remote", url); err != nil {
				return HandleError("failed to persist sync.remote to config.yaml: %v", err)
			}
			if isGitRepo() {
				commitBeadsConfig("bd: update sync.remote")
			}
		}

		if jsonOutput {
			return outputJSON(map[string]interface{}{
				"name": name,
				"url":  url,
			})
		}
		fmt.Printf("Added remote %q → %s\n", name, url)
		return nil
	},
}

var doltRemoteListCmd = &cobra.Command{
	Use:           "list",
	Short:         "List configured Dolt remotes",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-remote-list")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		ctx := context.Background()
		st := getStore()
		if st == nil {
			fmt.Fprintf(os.Stderr, "Error: no store available\n")
			return SilentExit()
		}

		remotes, err := st.ListRemotes(ctx)
		if err != nil {
			if jsonOutput {
				return outputJSONError(err, "remote_list_failed")
			}
			fmt.Fprintf(os.Stderr, "Error listing remotes: %v\n", err)
			return SilentExit()
		}

		if jsonOutput {
			return outputJSON(formatDoltRemoteListJSON(remotes))
		}

		if len(remotes) == 0 {
			fmt.Println("No remotes configured.")
			return nil
		}

		for _, r := range remotes {
			fmt.Printf("%-20s %s\n", r.Name, r.URL)
		}
		return nil
	},
}

type doltRemoteListJSON struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SQLURL string `json:"sql_url,omitempty"`
	CLIURL string `json:"cli_url,omitempty"`
	Status string `json:"status"`
}

func formatDoltRemoteListJSON(remotes []storage.RemoteInfo) []doltRemoteListJSON {
	out := make([]doltRemoteListJSON, 0, len(remotes))
	for _, r := range remotes {
		out = append(out, doltRemoteListJSON{
			Name:   r.Name,
			URL:    r.URL,
			SQLURL: r.URL,
			Status: "ok",
		})
	}
	return out
}

var doltRemoteRemoveCmd = &cobra.Command{
	Use:           "remove <name>",
	Short:         "Remove a Dolt remote",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dolt-remote-remove")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		ctx := context.Background()
		st := getStore()
		if st == nil {
			fmt.Fprintf(os.Stderr, "Error: no store available\n")
			return SilentExit()
		}
		name := args[0]

		if err := st.RemoveRemote(ctx, name); err != nil {
			if jsonOutput {
				return outputJSONError(err, "remote_remove_failed")
			}
			fmt.Fprintf(os.Stderr, "Error removing remote: %v\n", err)
			return SilentExit()
		}

		if name == "origin" {
			if current := config.GetYamlConfig("sync.remote"); current != "" {
				if err := config.UnsetYamlConfig("sync.remote"); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to clear sync.remote from config.yaml: %v\n", err)
				}
				if isGitRepo() {
					commitBeadsConfig("bd: clear sync.remote")
				}
			}
		}

		if jsonOutput {
			return outputJSON(map[string]interface{}{
				"name":    name,
				"removed": true,
			})
		}
		fmt.Printf("Removed remote %q\n", name)
		return nil
	},
}

// isTimeoutError checks if an error is a context deadline exceeded or timeout.
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if err == context.DeadlineExceeded {
		return true
	}
	// Check for net.Error timeout (covers TCP and MySQL driver timeouts)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Also catch wrapped context.DeadlineExceeded
	return errors.Is(err, context.DeadlineExceeded)
}

func init() {
	doltSetCmd.Flags().Bool("update-config", false, "Also write to config.yaml for team-wide defaults")
	doltStopCmd.Flags().Bool("force", false, "Force stop the server")
	doltPushCmd.Flags().Bool("force", false, "Force push (overwrite remote changes)")
	doltPushCmd.Flags().String("remote", "", "Push to a specific named remote instead of the default")
	doltPullCmd.Flags().String("remote", "", "Pull from a specific named remote instead of the default")
	doltCommitCmd.Flags().StringP("message", "m", "", "Commit message (default: auto-generated)")
	doltCleanDatabasesCmd.Flags().Bool("dry-run", false, "Show what would be dropped without dropping")
	doltRemoteCmd.AddCommand(doltRemoteAddCmd)
	doltRemoteCmd.AddCommand(doltRemoteListCmd)
	doltRemoteCmd.AddCommand(doltRemoteRemoveCmd)
	doltCmd.AddCommand(doltShowCmd)
	doltCmd.AddCommand(doltSetCmd)
	doltCmd.AddCommand(doltTestCmd)
	doltCmd.AddCommand(doltCommitCmd)
	doltCmd.AddCommand(doltPushCmd)
	doltCmd.AddCommand(doltPullCmd)
	doltCmd.AddCommand(doltStartCmd)
	doltCmd.AddCommand(doltStopCmd)
	doltCmd.AddCommand(doltStatusCmd)
	doltCmd.AddCommand(doltKillallCmd)
	doltCmd.AddCommand(doltCleanDatabasesCmd)
	doltCmd.AddCommand(doltRemoteCmd)
	rootCmd.AddCommand(doltCmd)
}

func selectedDoltBeadsDir() string {
	beadsDir := ""
	if os.Getenv("BEADS_DIR") != "" {
		beadsDir = beads.FindBeadsDir()
	}
	if beadsDir == "" {
		beadsDir = selectedNoDBBeadsDir(nil)
	}
	if beadsDir == "" {
		return ""
	}
	prepareSelectedNoDBContext(beadsDir)
	return beadsDir
}

func showDoltConfig(testConnection bool) error {
	beadsDir := selectedDoltBeadsDir()
	if beadsDir == "" {
		return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}

	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return SilentExit()
	}
	if cfg == nil {
		cfg = configfile.DefaultConfig()
	}

	backend := cfg.GetBackend()
	embedded := !usesSQLServer()

	// Resolve actual server port for connection testing
	showHost := cfg.GetDoltServerHost()
	dsCfg := doltserver.DefaultConfig(beadsDir)
	showPort := dsCfg.Port
	embeddedDataDir := filepath.Join(beadsDir, "embeddeddolt")

	if jsonOutput {
		result := map[string]interface{}{
			"backend": backend,
		}
		if backend == configfile.BackendDolt {
			result["database"] = cfg.GetDoltDatabase()
			result["embedded"] = embedded
			if embedded {
				result["data_dir"] = embeddedDataDir
			} else {
				result["host"] = showHost
				result["port"] = showPort
				result["user"] = cfg.GetDoltServerUser()
				result["shared_server"] = doltserver.IsSharedServerMode()
				if testConnection {
					result["connection_ok"] = testServerConnection(showHost, showPort)
				}
			}
		}
		return outputJSON(result)
	}

	if backend != configfile.BackendDolt {
		fmt.Printf("Backend: %s\n", backend)
		return nil
	}

	fmt.Println("Dolt Configuration")
	fmt.Println("==================")
	fmt.Printf("  Database: %s\n", cfg.GetDoltDatabase())
	if embedded {
		fmt.Println("  Mode:     embedded (in-process Dolt engine)")
		fmt.Printf("  Data:     %s\n", embeddedDataDir)
	} else {
		fmt.Printf("  Host:     %s\n", showHost)
		fmt.Printf("  Port:     %d\n", showPort)
		fmt.Printf("  User:     %s\n", cfg.GetDoltServerUser())
		if doltserver.IsSharedServerMode() {
			fmt.Println("  Mode:     shared server")
			if sharedDir, err := doltserver.SharedServerDir(); err == nil {
				fmt.Printf("  Server:   %s\n", sharedDir)
			}
		} else {
			fmt.Println("  Mode:     per-project")
		}

		if testConnection {
			fmt.Println()
			if testServerConnection(showHost, showPort) {
				fmt.Printf("  %s\n", ui.RenderPass("✓ Server connection OK"))
			} else {
				fmt.Printf("  %s\n", ui.RenderWarn("✗ Server not reachable"))
			}
		}
	}

	fmt.Println("\nRemotes:")
	ctx := context.Background()
	st := getStore()
	var remotes []storage.RemoteInfo
	if st != nil {
		remotes, _ = st.ListRemotes(ctx)
	}
	if len(remotes) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, r := range remotes {
			fmt.Printf("  %-16s %s\n", r.Name, r.URL)
		}
	}

	fmt.Println("\nConfig sources (priority order):")
	fmt.Println("  1. Environment variables (BEADS_DOLT_*)")
	fmt.Println("  2. metadata.json (local, gitignored)")
	fmt.Println("  3. config.yaml (team defaults)")
	return nil
}

func setDoltConfig(key, value string, updateConfig bool) error {
	beadsDir := selectedDoltBeadsDir()
	if beadsDir == "" {
		return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}

	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return SilentExit()
	}
	if cfg == nil {
		cfg = configfile.DefaultConfig()
	}

	if cfg.GetBackend() != configfile.BackendDolt {
		fmt.Fprintf(os.Stderr, "Error: not using Dolt backend\n")
		return SilentExit()
	}

	var yamlKey string

	switch key {
	case "mode":
		fmt.Fprintf(os.Stderr, "Error: mode is not yet configurable; embedded mode is coming soon\n")
		return SilentExit()

	case "database":
		if value == "" {
			fmt.Fprintf(os.Stderr, "Error: database name cannot be empty\n")
			return SilentExit()
		}
		cfg.DoltDatabase = value
		yamlKey = "dolt.database"

	case "host":
		if value == "" {
			fmt.Fprintf(os.Stderr, "Error: host cannot be empty\n")
			return SilentExit()
		}
		cfg.DoltServerHost = value
		yamlKey = "dolt.host"

	case "port":
		port, err := strconv.Atoi(value)
		if err != nil || port <= 0 || port > 65535 {
			fmt.Fprintf(os.Stderr, "Error: port must be a valid port number (1-65535)\n")
			return SilentExit()
		}
		cfg.DoltServerPort = port
		yamlKey = "dolt.port"

	case "socket":
		cfg.DoltServerSocket = value
		yamlKey = "dolt.socket"

	case "user":
		if value == "" {
			fmt.Fprintf(os.Stderr, "Error: user cannot be empty\n")
			return SilentExit()
		}
		cfg.DoltServerUser = value
		yamlKey = "dolt.user"

	case "data-dir":
		if value != "" && cfg.IsDoltServerMode() {
			fmt.Fprintf(os.Stderr, "Error: setting data-dir in server mode is not supported (GH#2438).\n")
			fmt.Fprintf(os.Stderr, "In server mode, the database is determined by the 'database' config key,\n")
			fmt.Fprintf(os.Stderr, "not the local data directory. Setting data-dir would silently disconnect\n")
			fmt.Fprintf(os.Stderr, "from the configured database '%s'.\n", cfg.GetDoltDatabase())
			fmt.Fprintf(os.Stderr, "\nTo change which database to use:\n")
			fmt.Fprintf(os.Stderr, "  bd dolt set database <name>\n")
			return SilentExit()
		}
		if value == "" {
			cfg.DoltDataDir = ""
		} else {
			if !filepath.IsAbs(value) {
				fmt.Fprintf(os.Stderr, "Error: data-dir must be an absolute path\n")
				return SilentExit()
			}
			cfg.DoltDataDir = value
			fmt.Fprintf(os.Stderr, "Note: absolute paths are not saved to metadata.json (it propagates via git).\n")
			fmt.Fprintf(os.Stderr, "For persistence, add to your shell profile:\n")
			fmt.Fprintf(os.Stderr, "  export BEADS_DOLT_DATA_DIR=%s\n", value)
		}
		yamlKey = "dolt.data-dir"

	case "shared-server":
		lower := strings.ToLower(value)
		if lower != "true" && lower != "false" {
			fmt.Fprintf(os.Stderr, "Error: shared-server must be 'true' or 'false'\n")
			return SilentExit()
		}
		if err := config.SetYamlConfig("dolt.shared-server", lower); err != nil {
			fmt.Fprintf(os.Stderr, "Error setting shared-server: %v\n", err)
			return SilentExit()
		}
		if jsonOutput {
			return outputJSON(map[string]interface{}{
				"key":      "shared-server",
				"value":    lower,
				"location": "config.yaml",
			})
		}
		if lower == "true" {
			fmt.Println("Shared server mode enabled.")
			fmt.Println("All projects will use a single Dolt server at ~/.beads/shared-server/.")
			fmt.Println("Each project's data remains isolated in its own database.")
		} else {
			fmt.Println("Shared server mode disabled. Each project will use its own Dolt server.")
		}
		return nil

	default:
		fmt.Fprintf(os.Stderr, "Error: unknown key '%s'\n", key)
		fmt.Fprintf(os.Stderr, "Valid keys: database, host, port, socket, user, data-dir, shared-server\n")
		return SilentExit()
	}

	logDoltConfigChange(beadsDir, key, value)

	if err := cfg.Save(beadsDir); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
		return SilentExit()
	}

	if jsonOutput {
		result := map[string]interface{}{
			"key":      key,
			"value":    value,
			"location": "metadata.json",
		}
		if updateConfig {
			result["config_yaml_updated"] = true
		}
		return outputJSON(result)
	}

	fmt.Printf("Set %s = %s (in metadata.json)\n", key, value)

	if updateConfig && yamlKey != "" {
		if err := config.SetYamlConfig(yamlKey, value); err != nil {
			fmt.Printf("%s\n", ui.RenderWarn(fmt.Sprintf("Warning: failed to update config.yaml: %v", err)))
		} else {
			fmt.Printf("Set %s = %s (in config.yaml)\n", yamlKey, value)
		}
	}
	return nil
}

func testDoltConnection() error {
	beadsDir := selectedDoltBeadsDir()
	if beadsDir == "" {
		return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}

	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return SilentExit()
	}
	if cfg == nil {
		cfg = configfile.DefaultConfig()
	}

	if cfg.GetBackend() != configfile.BackendDolt {
		fmt.Fprintf(os.Stderr, "Error: not using Dolt backend\n")
		return SilentExit()
	}

	host := cfg.GetDoltServerHost()
	port := doltserver.DefaultConfig(beadsDir).Port
	addr := fmt.Sprintf("%s:%d", host, port)

	if jsonOutput {
		ok := testServerConnection(host, port)
		if err := outputJSON(map[string]interface{}{
			"host":          host,
			"port":          port,
			"connection_ok": ok,
		}); err != nil {
			return err
		}
		if !ok {
			return SilentExit()
		}
		return nil
	}

	fmt.Printf("Testing connection to %s...\n", addr)

	if testServerConnection(host, port) {
		fmt.Printf("%s\n", ui.RenderPass("✓ Connection successful"))
	} else {
		fmt.Printf("%s\n", ui.RenderWarn("✗ Connection failed"))
		fmt.Println("\nStart the server with: bd dolt start")
		return SilentExit()
	}

	st := getStore()
	if st == nil {
		return nil
	}
	ctx := context.Background()
	remotes, err := st.ListRemotes(ctx)
	if err != nil || len(remotes) == 0 {
		return nil
	}
	fmt.Println("\nRemote connectivity:")
	for _, r := range remotes {
		if doltutil.IsSSHURL(r.URL) {
			// Test SSH connectivity by parsing host from URL
			sshHost := extractSSHHost(r.URL)
			if sshHost != "" {
				fmt.Printf("  %s (%s)... ", r.Name, r.URL)
				if testSSHConnectivity(sshHost) {
					fmt.Printf("%s\n", ui.RenderPass("✓ reachable"))
				} else {
					fmt.Printf("%s\n", ui.RenderWarn("✗ unreachable"))
				}
			}
		} else if strings.HasPrefix(r.URL, "https://") || strings.HasPrefix(r.URL, "http://") {
			fmt.Printf("  %s (%s)... ", r.Name, r.URL)
			if testHTTPConnectivity(r.URL) {
				fmt.Printf("%s\n", ui.RenderPass("✓ reachable"))
			} else {
				fmt.Printf("%s\n", ui.RenderWarn("✗ unreachable"))
			}
		} else {
			fmt.Printf("  %s (%s)... skipped (no connectivity test for this scheme)\n", r.Name, r.URL)
		}
	}
	return nil
}

// serverDialTimeout controls the TCP dial timeout for server connection tests.
// Tests may reduce this to avoid slow unreachable-host hangs in CI.
var serverDialTimeout = 3 * time.Second

func testServerConnection(host string, port int) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	conn, err := net.DialTimeout("tcp", addr, serverDialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close() // Best effort cleanup
	return true
}

// extractSSHHost extracts the hostname from an SSH URL for connectivity testing.
func extractSSHHost(url string) string {
	// git+ssh://git@github.com/org/repo.git → github.com
	// ssh://git@github.com/org/repo.git → github.com
	// git@github.com:org/repo.git → github.com
	url = strings.TrimPrefix(url, "git+ssh://")
	url = strings.TrimPrefix(url, "ssh://")
	if idx := strings.Index(url, "@"); idx >= 0 {
		url = url[idx+1:]
	}
	// Handle colon-separated (git@host:path) or slash-separated (ssh://host/path)
	if idx := strings.Index(url, ":"); idx >= 0 && !strings.Contains(url[:idx], "/") {
		return url[:idx]
	}
	if idx := strings.Index(url, "/"); idx >= 0 {
		return url[:idx]
	}
	return url
}

// testSSHConnectivity tests if an SSH host is reachable on port 22.
func testSSHConnectivity(host string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "22"), 5*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// httpURLToTCPAddr extracts a TCP dial address (host:port) from an HTTP(S) URL.
// Handles IPv6 addresses correctly (e.g., https://[::1]:8080/path).
func httpURLToTCPAddr(url string) string {
	host := url
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if idx := strings.Index(host, "/"); idx >= 0 {
		host = host[:idx]
	}
	defaultPort := "443"
	if strings.HasPrefix(url, "http://") {
		defaultPort = "80"
	}
	// Use net.SplitHostPort to correctly handle IPv6 addresses (which
	// contain colons that would otherwise be confused with host:port).
	if h, p, err := net.SplitHostPort(host); err == nil {
		return net.JoinHostPort(h, p)
	}
	// No port in host string. Strip IPv6 brackets if present so
	// JoinHostPort can re-add them correctly.
	h := strings.TrimPrefix(host, "[")
	h = strings.TrimSuffix(h, "]")
	return net.JoinHostPort(h, defaultPort)
}

// testHTTPConnectivity tests if an HTTP(S) URL is reachable via TCP.
func testHTTPConnectivity(url string) bool {
	addr := httpURLToTCPAddr(url)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// openDoltServerConnection opens a direct MySQL connection to the Dolt server
// using config from the beads directory. This bypasses getStore() which isn't
// initialized for dolt subcommands (beads-9vt). Connects without selecting a
// database so callers can operate on all databases (SHOW DATABASES, DROP DATABASE).
func openDoltServerConnection() (*sql.DB, func(), error) {
	beadsDir := selectedDoltBeadsDir()
	if beadsDir == "" {
		return nil, func() {}, HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}

	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return nil, func() {}, SilentExit()
	}
	if cfg == nil {
		cfg = configfile.DefaultConfig()
	}

	host := cfg.GetDoltServerHost()
	port := doltserver.DefaultConfig(beadsDir).Port
	user := cfg.GetDoltServerUser()
	password := os.Getenv("BEADS_DOLT_PASSWORD")

	connStr := doltutil.ServerDSN{
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
		TLS:      cfg.GetDoltServerTLS(),
	}.String()

	db, err := sql.Open("mysql", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to Dolt server: %v\n", err)
		return nil, func() {}, SilentExit()
	}

	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		fmt.Fprintf(os.Stderr, "Error: cannot reach Dolt server at %s:%d: %v\n", host, port, err)
		fmt.Fprintln(os.Stderr, "Start the server with: bd dolt start")
		return nil, func() {}, SilentExit()
	}

	return db, func() { _ = db.Close() }, nil
}

// doltServerPidFile returns the path to the PID file for the managed dolt server.
// logDoltConfigChange appends an audit entry to .beads/dolt-config.log.
// Includes the beadsDir path for debugging worktree config pollution (bd-la2cl).
func logDoltConfigChange(beadsDir, key, value string) {
	logPath := filepath.Join(beadsDir, "dolt-config.log")
	actor := os.Getenv("BEADS_ACTOR")
	if actor == "" {
		actor = os.Getenv("BD_ACTOR") // deprecated fallback
	}
	if actor == "" {
		actor = "unknown"
	}
	entry := fmt.Sprintf("%s actor=%s key=%s value=%s beads_dir=%s\n",
		time.Now().UTC().Format(time.RFC3339), actor, key, value, beadsDir)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return // best effort
	}
	defer f.Close()
	_, _ = f.WriteString(entry)
}
