package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Starchaser1024/LoomCode/internal/config"
	"github.com/Starchaser1024/LoomCode/internal/permissions"
)

func TestNewSingleVsMultiProviderState(t *testing.T) {
	single := New([]config.ProviderConfig{{Name: "p1", Model: "claude-sonnet-4-6"}}, nil, nil)
	if single.state != stateChat {
		t.Errorf("single provider should start in stateChat, got %v", single.state)
	}

	multi := New([]config.ProviderConfig{
		{Name: "p1", Model: "claude-sonnet-4-6"},
		{Name: "p2", Model: "gpt-4.1"},
	}, nil, nil)
	if multi.state != stateProviderSelect {
		t.Errorf("multiple providers should start in stateProviderSelect, got %v", multi.state)
	}
}

func TestRenderBannerShowsLoomCode(t *testing.T) {
	m := New([]config.ProviderConfig{{Name: "anthropic", Model: "claude-sonnet-4-6"}}, nil, nil)
	banner := m.renderBanner()
	if !strings.Contains(banner, "LoomCode") {
		t.Errorf("banner should contain LoomCode, got:\n%s", banner)
	}
	if !strings.Contains(banner, "claude-sonnet-4-6") {
		t.Errorf("banner should contain model name, got:\n%s", banner)
	}
}

func TestNextPermissionModeCycle(t *testing.T) {
	mode := permissions.ModeDefault
	mode = nextPermissionMode(mode)
	if mode != permissions.ModeAcceptEdits {
		t.Fatalf("expected acceptEdits, got %s", mode)
	}
	mode = nextPermissionMode(mode)
	if mode != permissions.ModePlan {
		t.Fatalf("expected plan, got %s", mode)
	}
	mode = nextPermissionMode(mode)
	if mode != permissions.ModeBypass {
		t.Fatalf("expected bypassPermissions, got %s", mode)
	}
	mode = nextPermissionMode(mode)
	if mode != permissions.ModeDefault {
		t.Fatalf("expected default, got %s", mode)
	}
}

func TestToolTitleFormatting(t *testing.T) {
	if got := toolTitle("ReadFile", map[string]any{"file_path": "/a/b/main.go"}); got != "Read main.go" {
		t.Errorf("ReadFile title = %q", got)
	}
	if got := toolTitle("WriteFile", map[string]any{"file_path": "/a/out.txt", "content": "a\nb"}); got != "Write out.txt (2 lines)" {
		t.Errorf("WriteFile title = %q", got)
	}
	if got := toolTitle("Bash", map[string]any{"command": "go test ./..."}); got != "Bash: go test ./..." {
		t.Errorf("Bash title = %q", got)
	}
}

func TestScanFilesSkipsLoomCodeDir(t *testing.T) {
	tmp := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(tmp)

	os.MkdirAll(filepath.Join(tmp, ".loomcode"), 0o755)
	os.MkdirAll(filepath.Join(tmp, "internal"), 0o755)
	os.WriteFile(filepath.Join(tmp, "main.go"), []byte("package main"), 0o644)

	matches := scanFiles("", 10)
	for _, m := range matches {
		if strings.Contains(m, ".loomcode") {
			t.Errorf(".loomcode should be skipped by scanFiles, got %v", matches)
		}
	}
}
