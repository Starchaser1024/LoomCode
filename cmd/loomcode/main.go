// Command loomcode launches the LoomCode terminal AI coding harness or runs as
// a headless teammate worker when invoked with --teammate.
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

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

	cfg, err := config.LoadConfig("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}

	// Validate hook config before handing it to the TUI. Invalid hooks are reported and dropped so a
	// configuration typo does not prevent interactive startup.
	validHooks := cfg.Hooks
	if err := hooks.Validate(validHooks); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: hook configuration is invalid, starting with no hooks:\n%s\n", err)
		validHooks = nil
	}

	m := tui.New(cfg.Providers, cfg.MCPServers, validHooks)
	p := tea.NewProgram(m)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}
