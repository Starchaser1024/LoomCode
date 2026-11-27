package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Starchaser1024/LoomCode/internal/filehistory"
)

func TestReadFileFullAndSlicing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	content := "alpha\nbeta\ngamma\ndelta\nepsilon"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadFileTool{}

	full := tool.Execute(context.Background(), map[string]any{"file_path": path})
	if full.IsError {
		t.Fatalf("unexpected ReadFile error: %s", full.Output)
	}
	wantFull := "1\talpha\n2\tbeta\n3\tgamma\n4\tdelta\n5\tepsilon"
	if full.Output != wantFull {
		t.Errorf("full read = %q, want %q", full.Output, wantFull)
	}

	// Verify offset + limit with JSON-decoded float64 numbers.
	sliced := tool.Execute(context.Background(), map[string]any{
		"file_path": path,
		"offset":    float64(1),
		"limit":     float64(2),
	})
	if sliced.IsError {
		t.Fatalf("unexpected sliced ReadFile error: %s", sliced.Output)
	}
	wantSlice := "2\tbeta\n3\tgamma"
	if sliced.Output != wantSlice {
		t.Errorf("sliced read = %q, want %q", sliced.Output, wantSlice)
	}

	// Offset past EOF returns empty string without error.
	pastEOF := tool.Execute(context.Background(), map[string]any{
		"file_path": path,
		"offset":    20,
		"limit":     5,
	})
	if pastEOF.IsError || pastEOF.Output != "" {
		t.Errorf("past-EOF read = (%q, err=%v), want empty non-error", pastEOF.Output, pastEOF.IsError)
	}
}

func TestReadFileErrorsAndCachePopulation(t *testing.T) {
	dir := t.TempDir()
	fsc := NewFileStateCache()
	tool := &ReadFileTool{FileStateCache: fsc}

	if res := tool.Execute(context.Background(), map[string]any{}); !res.IsError {
		t.Error("expected missing file_path to fail")
	}

	missing := filepath.Join(dir, "does_not_exist.go")
	if res := tool.Execute(context.Background(), map[string]any{"file_path": missing}); !res.IsError || !strings.Contains(res.Output, "file not found") {
		t.Errorf("expected file not found error, got: %+v", res)
	}

	if res := tool.Execute(context.Background(), map[string]any{"file_path": dir}); !res.IsError || !strings.Contains(res.Output, "not a file") {
		t.Errorf("expected directory read error, got: %+v", res)
	}

	target := filepath.Join(dir, "cached.txt")
	if err := os.WriteFile(target, []byte("hello cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if ok, _ := fsc.Check(target); ok {
		t.Fatal("FileStateCache.Check should fail before ReadFile")
	}

	res := tool.Execute(context.Background(), map[string]any{"file_path": target})
	if res.IsError {
		t.Fatalf("ReadFile failed: %s", res.Output)
	}
	if ok, msg := fsc.Check(target); !ok {
		t.Fatalf("FileStateCache.Check should succeed after ReadFile, got: %s", msg)
	}

	fsc.Clear()
	if ok, _ := fsc.Check(target); ok {
		t.Fatal("FileStateCache.Check should fail after Clear()")
	}
}

func TestWriteFileAndEditFileReadBeforeEditAndStaleness(t *testing.T) {
	dir := t.TempDir()
	fsc := NewFileStateCache()
	readTool := &ReadFileTool{FileStateCache: fsc}
	writeTool := &WriteFileTool{FileStateCache: fsc}
	editTool := &EditFileTool{FileStateCache: fsc}

	// Creating a new file with missing parent directories bypasses read-before-edit and populates cache.
	nestedNew := filepath.Join(dir, "a", "b", "c", "created.go")
	wRes := writeTool.Execute(context.Background(), map[string]any{
		"file_path": nestedNew,
		"content":   "package c\n\nconst V = 1\n",
	})
	if wRes.IsError {
		t.Fatalf("WriteFile on new nested path failed: %s", wRes.Output)
	}
	// Follow-up EditFile on newly written file succeeds because WriteFile updated FileStateCache.
	eRes := editTool.Execute(context.Background(), map[string]any{
		"file_path":  nestedNew,
		"old_string": "const V = 1",
		"new_string": "const V = 2",
	})
	if eRes.IsError {
		t.Fatalf("EditFile after WriteFile should succeed via updated cache: %s", eRes.Output)
	}

	// Existing unread file must be rejected by both EditFile and WriteFile.
	existing := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(existing, []byte("original content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	unreadEdit := editTool.Execute(context.Background(), map[string]any{
		"file_path":  existing,
		"old_string": "original",
		"new_string": "mutated",
	})
	if !unreadEdit.IsError || !strings.Contains(unreadEdit.Output, "not been read") {
		t.Fatalf("expected unread EditFile rejection, got: %+v", unreadEdit)
	}

	unreadWrite := writeTool.Execute(context.Background(), map[string]any{
		"file_path": existing,
		"content":   "overwritten without reading\n",
	})
	if !unreadWrite.IsError || !strings.Contains(unreadWrite.Output, "not been read") {
		t.Fatalf("expected unread WriteFile overwrite rejection, got: %+v", unreadWrite)
	}

	// Read the file, then simulate an external modification with a newer mtime.
	if r := readTool.Execute(context.Background(), map[string]any{"file_path": existing}); r.IsError {
		t.Fatalf("ReadFile failed: %s", r.Output)
	}
	if err := os.WriteFile(existing, []byte("externally modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(existing, future, future); err != nil {
		t.Fatal(err)
	}

	staleEdit := editTool.Execute(context.Background(), map[string]any{
		"file_path":  existing,
		"old_string": "externally",
		"new_string": "agent",
	})
	if !staleEdit.IsError || !strings.Contains(staleEdit.Output, "modified since last read") {
		t.Fatalf("expected stale EditFile rejection, got: %+v", staleEdit)
	}

	staleWrite := writeTool.Execute(context.Background(), map[string]any{
		"file_path": existing,
		"content":   "overwrite stale\n",
	})
	if !staleWrite.IsError || !strings.Contains(staleWrite.Output, "modified since last read") {
		t.Fatalf("expected stale WriteFile rejection, got: %+v", staleWrite)
	}

	// Re-reading clears staleness and permits editing.
	if r := readTool.Execute(context.Background(), map[string]any{"file_path": existing}); r.IsError {
		t.Fatalf("re-ReadFile failed: %s", r.Output)
	}
	freshEdit := editTool.Execute(context.Background(), map[string]any{
		"file_path":  existing,
		"old_string": "externally modified",
		"new_string": "cleanly updated",
	})
	if freshEdit.IsError {
		t.Fatalf("EditFile after re-read failed: %s", freshEdit.Output)
	}
	gotBytes, _ := os.ReadFile(existing)
	if string(gotBytes) != "cleanly updated\n" {
		t.Errorf("file content = %q, want %q", string(gotBytes), "cleanly updated\n")
	}
}

func TestEditFileUniquenessAndReplaceAll(t *testing.T) {
	dir := t.TempDir()
	fsc := NewFileStateCache()
	readTool := &ReadFileTool{FileStateCache: fsc}
	editTool := &EditFileTool{FileStateCache: fsc}

	path := filepath.Join(dir, "dup.go")
	initial := "foo := 1\nbar := foo + 2\nreturn foo\n"
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	readTool.Execute(context.Background(), map[string]any{"file_path": path})

	// Empty old_string rejected.
	if res := editTool.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "",
		"new_string": "x",
	}); !res.IsError {
		t.Error("expected empty old_string to fail")
	}

	// Missing old_string rejected.
	if res := editTool.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "nonexistent_ident",
		"new_string": "x",
	}); !res.IsError || !strings.Contains(res.Output, "not found") {
		t.Errorf("expected old_string not found error, got: %+v", res)
	}

	// Non-unique old_string rejected when replace_all=false (default).
	dupRes := editTool.Execute(context.Background(), map[string]any{
		"file_path":   path,
		"old_string":  "foo",
		"new_string":  "count",
		"replace_all": false,
	})
	if !dupRes.IsError || !strings.Contains(dupRes.Output, "found 3 times") {
		t.Fatalf("expected non-unique rejection, got: %+v", dupRes)
	}
	diskUnchanged, _ := os.ReadFile(path)
	if string(diskUnchanged) != initial {
		t.Fatalf("rejected edit must not modify disk, got %q", string(diskUnchanged))
	}

	// replace_all=true replaces every occurrence and keeps cache valid for sequential edits.
	allRes := editTool.Execute(context.Background(), map[string]any{
		"file_path":   path,
		"old_string":  "foo",
		"new_string":  "count",
		"replace_all": true,
	})
	if allRes.IsError {
		t.Fatalf("EditFile with replace_all=true failed: %s", allRes.Output)
	}
	wantReplaced := "count := 1\nbar := count + 2\nreturn count\n"
	afterAll, _ := os.ReadFile(path)
	if string(afterAll) != wantReplaced {
		t.Errorf("after replace_all = %q, want %q", string(afterAll), wantReplaced)
	}

	seqRes := editTool.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "bar := count + 2",
		"new_string": "bar := count + 10",
	})
	if seqRes.IsError {
		t.Fatalf("sequential EditFile after replace_all failed: %s", seqRes.Output)
	}
}

func TestWriteAndEditFileHistoryTracking(t *testing.T) {
	dir := t.TempDir()
	fh := filehistory.New(dir, "sess-history-tools")
	fsc := NewFileStateCache()

	readTool := &ReadFileTool{FileStateCache: fsc}
	writeTool := &WriteFileTool{FileHistory: fh, FileStateCache: fsc}
	editTool := &EditFileTool{FileHistory: fh, FileStateCache: fsc}

	existing := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(existing, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(dir, "sub", "ephemeral.txt")

	fh.BeginTurn(0, "apply changes")
	readTool.Execute(context.Background(), map[string]any{"file_path": existing})

	if res := editTool.Execute(context.Background(), map[string]any{
		"file_path":  existing,
		"old_string": "v1",
		"new_string": "v2",
	}); res.IsError {
		t.Fatalf("EditFile failed: %s", res.Output)
	}
	if res := writeTool.Execute(context.Background(), map[string]any{
		"file_path": created,
		"content":   "temporary\n",
	}); res.IsError {
		t.Fatalf("WriteFile failed: %s", res.Output)
	}
	fh.MakeSnapshot(2, "turn complete")

	snaps := fh.GetSnapshots()
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots (baseline + post-turn), got %d", len(snaps))
	}

	res, err := fh.RewindSafe(0, false)
	if err != nil {
		t.Fatalf("RewindSafe(0) failed: %v", err)
	}
	if len(res.Restored) != 1 || len(res.Deleted) != 1 {
		t.Errorf("unexpected rewind result: %+v", res)
	}
	restoredBytes, err := os.ReadFile(existing)
	if err != nil || string(restoredBytes) != "v1\n" {
		t.Errorf("restored content = %q (err=%v), want %q", string(restoredBytes), err, "v1\n")
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Errorf("newly created file %s should be removed on rewind", created)
	}
}
