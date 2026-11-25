package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Starchaser1024/LoomCode/internal/conversation"
	"github.com/Starchaser1024/LoomCode/internal/filehistory"
	"github.com/Starchaser1024/LoomCode/internal/llm"
	"github.com/Starchaser1024/LoomCode/internal/permissions"
	"github.com/Starchaser1024/LoomCode/internal/tools"
)

type stepOutcome struct {
	events []llm.StreamEvent
	err    error
}

// turnScriptClient drives Agent.Run with deterministic, state-inspecting streaming turns.
type turnScriptClient struct {
	mu       sync.Mutex
	handlers []func(call int, msgs []conversation.Message) stepOutcome
	calls    int
}

func (c *turnScriptClient) Stream(ctx context.Context, conv *conversation.Manager, _ []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	c.mu.Lock()
	idx := c.calls
	c.calls++
	var fn func(int, []conversation.Message) stepOutcome
	if idx < len(c.handlers) {
		fn = c.handlers[idx]
	}
	c.mu.Unlock()

	msgs := conv.GetMessages()
	evCh := make(chan llm.StreamEvent, 32)
	errCh := make(chan error, 1)

	go func() {
		defer close(evCh)
		defer close(errCh)
		if fn == nil {
			evCh <- llm.TextDelta{Text: "completed"}
			evCh <- llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 32, OutputTokens: 8}}
			return
		}
		out := fn(idx, msgs)
		for _, ev := range out.events {
			select {
			case evCh <- ev:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
		if out.err != nil {
			errCh <- out.err
		}
	}()
	return evCh, errCh
}

type goFixtureHarness struct {
	t         *testing.T
	dir       string
	sessionID string
	dt        tools.DefaultTools
	history   *filehistory.History
	testHash  map[string]string
}

// newGoFixtureRepo creates an isolated Git repository initialized with a Go module and initial commit.
func newGoFixtureRepo(t *testing.T, sessionID string, files map[string]string) *goFixtureHarness {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	dir := t.TempDir()
	if _, hasMod := files["go.mod"]; !hasMod {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/fixture\n\ngo 1.22\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
	}

	testHash := make(map[string]string)
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		if strings.HasSuffix(rel, "_test.go") {
			testHash[full] = fileSHA256(t, full)
		}
	}

	if _, err := exec.LookPath("git"); err == nil {
		for _, args := range [][]string{
			{"init", "-q"},
			{"config", "user.email", "ci@loomcode.local"},
			{"config", "user.name", "loomcode-ci"},
			{"config", "commit.gpgsign", "false"},
			{"add", "."},
			{"-c", "commit.gpgsign=false", "commit", "-q", "-m", "fixture baseline"},
		} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			if out, runErr := cmd.CombinedOutput(); runErr != nil {
				t.Fatalf("git %v: %v\n%s", args, runErr, out)
			}
		}
	}

	dt := newScopedDefaultTools(dir)
	hist := filehistory.New(dir, sessionID)
	dt.EditFile.FileHistory = hist
	dt.WriteFile.FileHistory = hist

	return &goFixtureHarness{
		t:         t,
		dir:       dir,
		sessionID: sessionID,
		dt:        dt,
		history:   hist,
		testHash:  testHash,
	}
}

func newScopedDefaultTools(workDir string) tools.DefaultTools {
	dt := tools.CreateDefaultTools()
	dt.Registry.Register(&tools.BashTool{WorkDir: workDir})
	return dt
}

func (f *goFixtureHarness) newAgent(client llm.Client) *Agent {
	ag := New(client, f.dt.Registry, "anthropic")
	ag.WorkDir = f.dir
	ag.SessionID = f.sessionID
	ag.FileHistory = f.history
	ag.RetryBaseDelay = time.Millisecond
	ag.MaxIterations = 12
	ag.Checker = permissions.NewChecker(
		permissions.NewStrictPathSandbox(f.dir),
		nil,
		permissions.ModeBypass,
	)
	return ag
}

func (f *goFixtureHarness) path(rel string) string {
	return filepath.Join(f.dir, filepath.FromSlash(rel))
}

func (f *goFixtureHarness) runGoTest() (string, error) {
	cmd := exec.Command("go", "test", "-count=1", "./...")
	cmd.Dir = f.dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f *goFixtureHarness) assertBaselineFails() {
	f.t.Helper()
	if out, err := f.runGoTest(); err == nil {
		f.t.Fatalf("expected unpatched fixture to fail `go test ./...`, but it passed:\n%s", out)
	}
}

func (f *goFixtureHarness) assertTestsPassAndUntampered() {
	f.t.Helper()
	out, err := f.runGoTest()
	if err != nil {
		f.t.Fatalf("expected `go test ./...` to pass after fix, got err=%v:\n%s", err, out)
	}
	for testFile, wantHash := range f.testHash {
		gotHash := fileSHA256(f.t, testFile)
		if gotHash != wantHash {
			f.t.Fatalf("test file %s was tampered with (hash %s != baseline %s)", testFile, gotHash, wantHash)
		}
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func assertGoFileDeclaresSymbol(t *testing.T, path, symbolName string) {
	t.Helper()
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse Go AST %s: %v", path, err)
	}
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		switch decl := n.(type) {
		case *ast.FuncDecl:
			if decl.Name != nil && decl.Name.Name == symbolName {
				found = true
				return false
			}
		case *ast.TypeSpec:
			if decl.Name != nil && decl.Name.Name == symbolName {
				found = true
				return false
			}
		}
		return true
	})
	if !found {
		t.Fatalf("expected AST of %s to declare %q", path, symbolName)
	}
}

func lastToolResultText(msgs []conversation.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if len(msgs[i].ToolResults) > 0 {
			var parts []string
			for _, tr := range msgs[i].ToolResults {
				parts = append(parts, tr.Content)
			}
			return strings.Join(parts, "\n")
		}
	}
	return ""
}
