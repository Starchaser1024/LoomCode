package filehistory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMultiTurnEditAndRewind(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "service.go")
	if err := os.WriteFile(fileA, []byte("package app\n// v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := New(dir, "sess-multiturn")

	// Turn 1: v0 -> v1
	h.BeginTurn(0, "update service to v1")
	h.TrackEdit(fileA)
	if err := os.WriteFile(fileA, []byte("package app\n// v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.RecordWrite(fileA, []byte("package app\n// v1\n"))
	h.MakeSnapshot(2, "updated service to v1")

	// Turn 2: v1 -> v2
	h.TrackEdit(fileA)
	if err := os.WriteFile(fileA, []byte("package app\n// v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.RecordWrite(fileA, []byte("package app\n// v2\n"))
	h.MakeSnapshot(4, "updated service to v2")

	// Turn 3: v2 -> v3
	h.TrackEdit(fileA)
	if err := os.WriteFile(fileA, []byte("package app\n// v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.RecordWrite(fileA, []byte("package app\n// v3\n"))
	h.MakeSnapshot(6, "updated service to v3")

	snaps := h.GetSnapshots()
	if len(snaps) != 4 {
		t.Fatalf("expected 4 snapshots (initial + 3 turns), got %d", len(snaps))
	}

	// Rewind to Snapshot[1] (after Turn 1: v1)
	changed, err := h.Rewind(1)
	if err != nil {
		t.Fatalf("Rewind(1) failed: %v", err)
	}
	if len(changed) != 1 || changed[0] != fileA {
		t.Fatalf("expected [%s] changed, got %v", fileA, changed)
	}
	got, _ := os.ReadFile(fileA)
	if string(got) != "package app\n// v1\n" {
		t.Fatalf("expected v1 after Rewind(1), got %q", string(got))
	}

	// Rewind all the way to Snapshot[0] (initial baseline: v0)
	changed, err = h.Rewind(0)
	if err != nil {
		t.Fatalf("Rewind(0) failed: %v", err)
	}
	if len(changed) != 1 || changed[0] != fileA {
		t.Fatalf("expected [%s] changed on Rewind(0), got %v", fileA, changed)
	}
	got, _ = os.ReadFile(fileA)
	if string(got) != "package app\n// v0\n" {
		t.Fatalf("expected v0 after Rewind(0), got %q", string(got))
	}
}

func TestRewindDeletesCreatedFilesFromLaterTurns(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "main.go")
	helper1 := filepath.Join(dir, "internal", "helper1.go")
	helper2 := filepath.Join(dir, "internal", "helper2.go")

	if err := os.WriteFile(existing, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := New(dir, "sess-created")

	// Turn 1: edit existing file only -> Snapshot 0 (when BeginTurn not called)
	h.TrackEdit(existing)
	if err := os.WriteFile(existing, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.RecordWrite(existing, []byte("v1"))
	h.MakeSnapshot(2, "turn 1")

	// Turn 2: create helper1.go and edit existing -> Snapshot 1
	if err := os.MkdirAll(filepath.Dir(helper1), 0o755); err != nil {
		t.Fatal(err)
	}
	h.TrackEdit(helper1)
	if err := os.WriteFile(helper1, []byte("package internal\nfunc H1() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.RecordWrite(helper1, []byte("package internal\nfunc H1() {}\n"))
	h.TrackEdit(existing)
	if err := os.WriteFile(existing, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.RecordWrite(existing, []byte("v2"))
	h.MakeSnapshot(4, "turn 2")

	// Turn 3 (interrupted before MakeSnapshot): create helper2.go
	h.TrackEdit(helper2)
	if err := os.WriteFile(helper2, []byte("package internal\nfunc H2() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.RecordWrite(helper2, []byte("package internal\nfunc H2() {}\n"))

	// Rewind to Snapshot 0 (end of Turn 1): existing -> v1, both helper1 and helper2 deleted!
	res, err := h.RewindSafe(0, false)
	if err != nil {
		t.Fatalf("RewindSafe(0, false) error: %v", err)
	}
	if len(res.Restored) != 1 || res.Restored[0] != existing {
		t.Fatalf("expected Restored=[%s], got %v", existing, res.Restored)
	}
	if len(res.Deleted) != 2 {
		t.Fatalf("expected 2 deleted files, got %v", res.Deleted)
	}
	if _, err := os.Stat(helper1); !os.IsNotExist(err) {
		t.Fatalf("expected helper1.go to be deleted, stat err=%v", err)
	}
	if _, err := os.Stat(helper2); !os.IsNotExist(err) {
		t.Fatalf("expected helper2.go to be deleted, stat err=%v", err)
	}
	got, _ := os.ReadFile(existing)
	if string(got) != "v1" {
		t.Fatalf("expected existing=v1, got %q", string(got))
	}
}

func TestPersistenceAcrossReload(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")
	genFile := filepath.Join(dir, "generated.go")
	if err := os.WriteFile(cfgFile, []byte("port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h1 := New(dir, "sess-persist")
	h1.BeginTurn(0, "change port and add generated file")

	// Turn 1
	h1.TrackEdit(cfgFile)
	if err := os.WriteFile(cfgFile, []byte("port: 9090\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1.RecordWrite(cfgFile, []byte("port: 9090\n"))
	h1.MakeSnapshot(2, "port 9090")

	// Turn 2
	h1.TrackEdit(genFile)
	if err := os.WriteFile(genFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1.RecordWrite(genFile, []byte("package main\n"))
	h1.MakeSnapshot(4, "added generated.go")

	// Simulate process restart + /resume by constructing a fresh History with the same sessionID.
	h2 := New(dir, "sess-persist")
	if !h2.HasSnapshots() {
		t.Fatal("expected reloaded History to have snapshots")
	}
	if len(h2.GetSnapshots()) != 3 {
		t.Fatalf("expected 3 snapshots after reload, got %d", len(h2.GetSnapshots()))
	}

	// Rewind to Snapshot 0 (initial baseline before Turn 1) on the reloaded instance.
	res, err := h2.RewindSafe(0, false)
	if err != nil {
		t.Fatalf("reloaded RewindSafe(0, false) failed: %v", err)
	}
	if len(res.Restored) != 1 || res.Restored[0] != cfgFile {
		t.Fatalf("expected cfgFile restored, got %v", res.Restored)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != genFile {
		t.Fatalf("expected genFile deleted, got %v", res.Deleted)
	}
	data, _ := os.ReadFile(cfgFile)
	if string(data) != "port: 8080\n" {
		t.Fatalf("expected port: 8080, got %q", string(data))
	}
	if _, err := os.Stat(genFile); !os.IsNotExist(err) {
		t.Fatal("expected genFile to be removed after rewind")
	}
}

func TestRewindPreservesUserManualEditsUnlessForced(t *testing.T) {
	dir := t.TempDir()
	agentOnly := filepath.Join(dir, "agent_only.go")
	userEdited := filepath.Join(dir, "shared.go")
	createdUserEdited := filepath.Join(dir, "new_by_agent.go")

	if err := os.WriteFile(agentOnly, []byte("a0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userEdited, []byte("u0"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := New(dir, "sess-user-safety")
	h.BeginTurn(0, "refactor files")

	// Agent Turn 1 modifies agentOnly, userEdited, and creates createdUserEdited.
	h.TrackEdit(agentOnly)
	_ = os.WriteFile(agentOnly, []byte("a1"), 0o644)
	h.RecordWrite(agentOnly, []byte("a1"))

	h.TrackEdit(userEdited)
	_ = os.WriteFile(userEdited, []byte("u1-agent"), 0o644)
	h.RecordWrite(userEdited, []byte("u1-agent"))

	h.TrackEdit(createdUserEdited)
	_ = os.WriteFile(createdUserEdited, []byte("c1-agent"), 0o644)
	h.RecordWrite(createdUserEdited, []byte("c1-agent"))

	h.MakeSnapshot(2, "turn 1 complete")

	// Simulate user manually editing shared.go and new_by_agent.go in their editor.
	if err := os.WriteFile(userEdited, []byte("u2-manual-by-user"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(createdUserEdited, []byte("c2-manual-by-user"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Safe rewind to initial checkpoint (index 0) must restore agentOnly while skipping both user-modified files.
	res, err := h.RewindSafe(0, false)
	if err != nil {
		t.Fatalf("RewindSafe(0, false) failed: %v", err)
	}
	if len(res.Restored) != 1 || res.Restored[0] != agentOnly {
		t.Fatalf("expected Restored=[%s], got %v", agentOnly, res.Restored)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("expected 0 deleted (user edited created file), got %v", res.Deleted)
	}
	if len(res.SkippedUserModified) != 2 {
		t.Fatalf("expected 2 SkippedUserModified, got %v", res.SkippedUserModified)
	}

	if got, _ := os.ReadFile(agentOnly); string(got) != "a0" {
		t.Fatalf("agentOnly = %q, want a0", string(got))
	}
	if got, _ := os.ReadFile(userEdited); string(got) != "u2-manual-by-user" {
		t.Fatalf("userEdited was clobbered: got %q, want u2-manual-by-user", string(got))
	}
	if got, _ := os.ReadFile(createdUserEdited); string(got) != "c2-manual-by-user" {
		t.Fatalf("createdUserEdited was clobbered: got %q, want c2-manual-by-user", string(got))
	}

	// Explicit force=true overwrites/deletes even user-modified files when requested.
	forced, err := h.RewindSafe(0, true)
	if err != nil {
		t.Fatalf("RewindSafe(0, true) failed: %v", err)
	}
	if len(forced.SkippedUserModified) != 0 {
		t.Fatalf("expected 0 skipped with force=true, got %v", forced.SkippedUserModified)
	}
	if got, _ := os.ReadFile(userEdited); string(got) != "u0" {
		t.Fatalf("expected userEdited=u0 after forced rewind, got %q", string(got))
	}
	if _, err := os.Stat(createdUserEdited); !os.IsNotExist(err) {
		t.Fatalf("expected createdUserEdited deleted after forced rewind")
	}
}

func TestLoadLegacySnapshotsArray(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, ".loomcode", "file-history", "legacy-sess")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(dir, "legacy.txt")
	backupFile := filepath.Join(sessDir, "backup@v1")
	_ = os.WriteFile(backupFile, []byte("legacy-v1"), 0o644)
	_ = os.WriteFile(targetFile, []byte("legacy-v1"), 0o644)

	legacy := []Snapshot{{
		MessageIndex: 2,
		UserText:     "legacy checkpoint",
		Backups: map[string]Backup{
			targetFile: {BackupPath: backupFile, Version: 1, Time: time.Now()},
		},
		Timestamp: time.Now(),
	}}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(sessDir, snapshotsFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	h := New(dir, "legacy-sess")
	if len(h.GetSnapshots()) != 1 {
		t.Fatalf("expected 1 legacy snapshot loaded, got %d", len(h.GetSnapshots()))
	}
}
