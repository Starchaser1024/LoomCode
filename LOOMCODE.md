# LOOMCODE.md

This file provides architectural context, build/test commands, and engineering invariants for contributors and AI coding agents working on the **LoomCode** repository.

## Build & Verification Commands

- **Typecheck & static analysis**: `go vet ./...`
- **Run full unit test suite**: `go test ./...`
- **Run single package tests**: `go test -v ./internal/<pkg>/...`
- **Build CLI binary**: `go build ./cmd/loomcode` (avoid committing compiled binaries)
- **Live LLM integration tests** (opt-in only): `LOOMCODE_LIVE_TESTS=1 go test ./internal/agent/ -run TestLive -v -count=1`

## Architecture & Package Boundaries

- `cmd/loomcode`: Entry point only. Parses `--teammate` worker flags (`teammate.go`) or boots the Bubble Tea TUI (`main.go`). Keep business logic in `internal/`.
- `internal/agent`: Core streaming ReAct loop (`agent.go`), concurrent tool execution (`streaming_executor.go`), stream error/rate-limit recovery and per-tool permission/hook gating (`tool_exec.go`). Must not import `internal/tui`, `internal/agents`, or `internal/teams` (avoids dependency cycles).
- `internal/agents`: Sub-agent specs, `.loomcode/agents/*.md` loader, 6-layer tool visibility filter (`tool_filter.go`), `Agent` tool execution (`agent_tool.go`), and conversation forking / teammate dispatch (`agent_fork.go`).
- `internal/teams`: Multi-agent coordination (`CoordinatorAllowedTools`), atomic file-locked mailboxes (`filemailbox.go`), in-process goroutine runner, and `tmux` / `iTerm2` pane spawners.
- `internal/worktree`: Isolated Git worktrees under `.loomcode/worktrees/`. Uses zero-subprocess `.git` metadata parsing (`filesystem.go`) for fast session/agent worktree attachment.
- `internal/toolresult` & `internal/compact`: Two-layer context management:
  - Layer 1 (`toolresult`): Spills oversized tool outputs and freezes replacement previews in `ContentReplacementState` without mutating `conversation.Manager`.
  - Layer 2 (`compact`): Summarizes older message prefixes when usage crosses the effective window threshold while preserving recent tail messages, tool pairs, and `RecoveryState`.
- `internal/memory` & `internal/memory/extractor`: Discovers hierarchical instruction files (`LOOMCODE.md`, `CLAUDE.md`, `AGENTS.md`, `LOOMCODE.local.md`), manages dual-path user/project memories, recalls relevant memories via side-query selection, and runs background post-turn extraction.
- `internal/skills`: Three-tier `SKILL.md` catalog (`builtins` → `~/.loomcode/skills` → `.loomcode/skills`), two-phase hot reload, `inline`/`fork` execution, compiled directory tools (`tool.json` + `builtinToolFactories`), and atomic remote installation.
- `internal/tui`: Modular Bubble Tea UI split by responsibility (`tui.go`, `agent_bridge.go`, `stream.go`, `input.go`, `commands_exec.go`, `dialogs.go`, `sessions_ui.go`, `render.go`, `styles.go`, `verbs.go`).

## Key Engineering Invariants

1. **Prompt-Cache Prefix Stability**: Never mutate historical messages in-place across turns. Tool-result replacements in `internal/toolresult` must remain deterministic per `tool_use_id`, and forked sub-agents must preserve `ThinkingBlocks` and clone the parent's `ContentReplacementState`.
2. **Read-Before-Edit Safety**: `EditFile` and `WriteFile` coordinate with `FileStateCache` and `filehistory.History` so files are snapshotted for `/rewind` before modification.
3. **Cross-Platform Path Discipline**:
   - Always use `path.Join` (never `filepath.Join`) when addressing Go `embed.FS` paths (`internal/skills/builtins.go`).
   - Terminate upward directory walks with `parent := filepath.Dir(dir); if parent == dir { break }` rather than comparing against `"/"` so tests and discovery work on both Windows and Unix.
   - Normalize `Glob` relative output with `filepath.ToSlash` for consistent model consumption across OSes.
4. **Headless Sub-Agent Non-Blocking**: Spawned sub-agents and forked workers have no interactive UI; always auto-respond (`PermDeny`) to `PermissionRequestEvent` and use non-blocking sends on UI progress channels so cancellation (`ctx.Done()`) never deadlocks.
5. **Code Style**: Standard idiomatic Go (`gofmt`, `camelCase` / `PascalCase` identifiers, wrapped errors via `fmt.Errorf("...: %w", err)`), concise English commit messages in Conventional Commits format (`feat(scope): ...`, `fix(scope): ...`), and keep individual `.go` files focused (<650 lines).
