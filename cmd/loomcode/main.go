// Command loomcode launches the LoomCode terminal AI coding harness, executes a
// non-interactive task with -p/--prompt, or runs as a teammate worker with --teammate.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Starchaser1024/LoomCode/internal/agent"
	"github.com/Starchaser1024/LoomCode/internal/config"
	"github.com/Starchaser1024/LoomCode/internal/hooks"
	"github.com/Starchaser1024/LoomCode/internal/tui"
)

func main() {
	// Teammate-worker mode: when this binary is launched by teams.BuildTeammateCLI from a tmux/iTerm
	// spawn, the first arg is --teammate. Worker mode has no TUI; it runs the in-process teammate loop
	// against the shared mailbox.
	if args, ok := parseTeammateFlags(os.Args[1:]); ok {
		if err := runTeammate(args); err != nil {
			fmt.Fprintf(os.Stderr, "teammate: %s\n", err)
			os.Exit(1)
		}
		return
	}

	code := executeCLI(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, headlessDeps{}, true)
	if code != 0 {
		os.Exit(code)
	}
}

func executeCLI(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	deps headlessDeps,
	allowInteractive bool,
) int {
	opts, err := parseCLIFlags(args)
	if err != nil {
		format := "text"
		if wantsJSONOutput(args) {
			format = "json"
		}
		return emitHeadlessError(format, stdout, stderr, "", "", agent.ExecutionReceipt{}, err)
	}

	if opts.ShowHelp {
		printUsage(stdout)
		return 0
	}
	if opts.ShowVersion {
		fmt.Fprintf(stdout, "loomcode v%s\n", version)
		return 0
	}
	if opts.HasPrompt {
		return runHeadless(ctx, opts, stdin, stdout, stderr, deps)
	}

	if !allowInteractive {
		fmt.Fprintln(stderr, "Error: interactive TUI disabled in test harness")
		return 1
	}

	restoreDir, _, err := changeWorkingDir(opts.Cwd)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %s\n", err)
		return 1
	}
	defer restoreDir()

	cfg, err := config.LoadConfig("")
	if err != nil {
		fmt.Fprintf(stderr, "Error: %s\n", err)
		return 1
	}

	providers := cfg.Providers
	if opts.Provider != "" {
		selected, err := selectProvider(cfg.Providers, opts.Provider)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %s\n", err)
			return 1
		}
		providers = []config.ProviderConfig{*selected}
	}

	// Validate hook config before handing it to the TUI. Invalid hooks are reported and dropped so a
	// configuration typo does not prevent interactive startup.
	validHooks := cfg.Hooks
	if err := hooks.Validate(validHooks); err != nil {
		fmt.Fprintf(stderr, "Warning: hook configuration is invalid, starting with no hooks:\n%s\n", err)
		validHooks = nil
	}

	m := tui.New(providers, cfg.MCPServers, validHooks)
	if opts.PermissionMode != "" {
		if pm, err := parsePermissionMode(opts.PermissionMode); err == nil {
			m.SetInitialPermissionMode(pm)
		}
	} else if cfg.PermissionMode != "" {
		if pm, err := parsePermissionMode(cfg.PermissionMode); err == nil {
			m.SetInitialPermissionMode(pm)
		}
	}

	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(stderr, "Error: %s\n", err)
		return 1
	}
	return 0
}
