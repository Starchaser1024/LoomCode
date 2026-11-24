package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Starchaser1024/LoomCode/internal/tools"
)

func writeSkillDir(t *testing.T, root, name, frontmatter, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "---\n" + frontmatter + "\n---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return dir
}

func TestLoadCatalogPhase1EmptyBody(t *testing.T) {
	work := t.TempDir()
	writeSkillDir(t, filepath.Join(work, ".loomcode", "skills"), "demo",
		"name: demo\ndescription: a phase-1 demo\nmode: inline",
		"# Body\n\nFull SOP here.")

	cat := LoadCatalog(work)
	skill := cat.Get("demo")
	if skill == nil {
		t.Fatal("expected demo skill in phase-1 catalog")
	}
	if skill.PromptBody != "" {
		t.Errorf("phase-1 must NOT load body; got %d chars", len(skill.PromptBody))
	}
	if skill.BodyLoaded {
		t.Errorf("BodyLoaded must be false in phase-1")
	}
	if skill.Meta.Description != "a phase-1 demo" {
		t.Errorf("frontmatter not parsed: %q", skill.Meta.Description)
	}
}

func TestCatalogGetFullHotReload(t *testing.T) {
	work := t.TempDir()
	dir := writeSkillDir(t, filepath.Join(work, ".loomcode", "skills"), "hot",
		"name: hot\ndescription: hot reload demo",
		"version 1")

	cat := LoadCatalog(work)
	skill, err := cat.GetFull("hot")
	if err != nil {
		t.Fatalf("GetFull v1: %v", err)
	}
	if !strings.Contains(skill.PromptBody, "version 1") {
		t.Errorf("v1 body mismatch: %q", skill.PromptBody)
	}

	// Overwrite source file — GetFull must re-read.
	updated := "---\nname: hot\ndescription: hot reload demo\n---\n\nversion 2"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(updated), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	skill2, err := cat.GetFull("hot")
	if err != nil {
		t.Fatalf("GetFull v2: %v", err)
	}
	if !strings.Contains(skill2.PromptBody, "version 2") {
		t.Errorf("hot reload didn't pick up v2: %q", skill2.PromptBody)
	}
}

func TestLoadCatalogBuiltinsPresent(t *testing.T) {
	cat := LoadCatalog(t.TempDir())
	wantNames := []string{"commit", "test", "review"}
	for _, name := range wantNames {
		s := cat.Get(name)
		if s == nil {
			t.Errorf("builtin %q missing from catalog", name)
		}
		if cat.Source(name) != "builtin" {
			t.Errorf("builtin %q source = %q, want builtin", name, cat.Source(name))
		}
	}
}

func TestLoadCatalogProjectOverridesBuiltin(t *testing.T) {
	work := t.TempDir()
	writeSkillDir(t, filepath.Join(work, ".loomcode", "skills"), "commit",
		"name: commit\ndescription: project override",
		"project body")

	cat := LoadCatalog(work)
	if cat.Source("commit") != "project" {
		t.Errorf("project tier did not override builtin; source = %q", cat.Source("commit"))
	}
	if cat.Get("commit").Meta.Description != "project override" {
		t.Errorf("description not from project tier: %q", cat.Get("commit").Meta.Description)
	}
}

func TestBuiltinReviewIsDirectory(t *testing.T) {
	cat := LoadCatalog(t.TempDir())
	skill := cat.Get("review")
	if skill == nil {
		t.Fatal("review builtin missing")
	}
	if !skill.IsDirectory {
		t.Errorf("review should be IsDirectory (has tool.json)")
	}
	schemas, err := parseToolJSON(skill)
	if err != nil {
		t.Fatalf("parseToolJSON: %v", err)
	}
	if len(schemas) != 1 || schemas[0].Name != "diff_summary" {
		t.Errorf("expected single diff_summary schema, got %+v", schemas)
	}

	reg := tools.NewRegistry()
	n, err := RegisterDirectoryTools(skill, reg)
	if err != nil {
		t.Fatalf("RegisterDirectoryTools: %v", err)
	}
	if n != 1 || reg.Get("diff_summary") == nil {
		t.Fatalf("expected diff_summary registered, count=%d", n)
	}
}

func TestDiffSummaryToolParsesUnifiedDiff(t *testing.T) {
	sampleDiff := `diff --git a/internal/engine/worker.go b/internal/engine/worker.go
index 1111111..2222222 100644
--- a/internal/engine/worker.go
+++ b/internal/engine/worker.go
@@ -12,4 +12,7 @@ func RunWorker(ctx context.Context) error {
-	oldCall()
+	var mu sync.Mutex
+	go dispatchTask(ctx, &mu)
+	return nil
diff --git a/internal/engine/worker_test.go b/internal/engine/worker_test.go
deleted file mode 100644
--- a/internal/engine/worker_test.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package engine
`
	tool := builtinToolFactories["diff_summary"](ToolSchema{Name: "diff_summary"})
	res := tool.Execute(context.Background(), map[string]any{"diff": sampleDiff})
	if res.IsError {
		t.Fatalf("diff_summary failed: %s", res.Output)
	}
	var report DiffSummaryReport
	if err := json.Unmarshal([]byte(res.Output), &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if report.FilesChanged != 2 || report.TotalInsertions != 3 || report.TotalDeletions != 2 {
		t.Errorf("unexpected stats: %+v", report)
	}
	if len(report.ModifiedSymbols) == 0 || !strings.Contains(report.ModifiedSymbols[0], "RunWorker") {
		t.Errorf("expected RunWorker in modified symbols, got %v", report.ModifiedSymbols)
	}
	if len(report.RiskSignals) < 2 {
		t.Errorf("expected concurrency and deleted-test risk signals, got %v", report.RiskSignals)
	}
}
