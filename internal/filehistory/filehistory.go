// Package filehistory snapshots modified files per turn to support non-destructive /rewind checkpoints.
package filehistory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxSnapshots      = 100
	snapshotsFileName = "snapshots.json"
)

// Backup records the state of a tracked file at a specific Snapshot.
type Backup struct {
	BackupPath  string    `json:"backup_path,omitempty"`
	Version     int       `json:"version"`
	ContentHash string    `json:"content_hash,omitempty"`
	Existed     bool      `json:"existed"`
	Time        time.Time `json:"time"`
}

// FileOrigin captures the baseline state of a file right before the agent
// touched it for the first time in this session.
type FileOrigin struct {
	Existed     bool   `json:"existed"`
	BackupPath  string `json:"backup_path,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

// Snapshot represents a named conversation + workspace checkpoint.
type Snapshot struct {
	MessageIndex int               `json:"message_index"`
	UserText     string            `json:"user_text"`
	Backups      map[string]Backup `json:"backups"`
	Timestamp    time.Time         `json:"timestamp"`
}

// RewindResult reports the exact filesystem actions taken by RewindSafe.
type RewindResult struct {
	Restored            []string `json:"restored"`
	Deleted             []string `json:"deleted"`
	SkippedUserModified []string `json:"skipped_user_modified"`
}

// ChangedFiles returns a sorted list of all files modified on disk (restored + deleted).
func (r *RewindResult) ChangedFiles() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.Restored)+len(r.Deleted))
	out = append(out, r.Restored...)
	out = append(out, r.Deleted...)
	sort.Strings(out)
	return out
}

type pendingTurn struct {
	msgIndex int
	userText string
}

type persistedState struct {
	Snapshots     []Snapshot            `json:"snapshots"`
	TrackedFiles  map[string]int        `json:"tracked_files,omitempty"`
	Origins       map[string]FileOrigin `json:"origins,omitempty"`
	LastAgentHash map[string]string     `json:"last_agent_hash,omitempty"`
}

// History manages content-addressed file backups and checkpoints for a single session.
type History struct {
	mu              sync.Mutex
	sessionDir      string
	trackedFiles    map[string]int        // absPath -> current version
	origins         map[string]FileOrigin // absPath -> pre-session baseline
	lastAgentHash   map[string]string     // absPath -> SHA-256 last written/observed by agent
	snapshots       []Snapshot
	pendingBaseline *pendingTurn
}

// New initializes (or reloads) file history under <baseDir>/.loomcode/file-history/<sessionID>.
// If snapshots.json already exists on disk from an earlier run or interrupted process, New
// automatically restores checkpoints and file tracking metadata.
func New(baseDir, sessionID string) *History {
	dir := filepath.Join(baseDir, ".loomcode", "file-history", sessionID)
	_ = os.MkdirAll(dir, 0o755)
	h := &History{
		sessionDir:    dir,
		trackedFiles:  make(map[string]int),
		origins:       make(map[string]FileOrigin),
		lastAgentHash: make(map[string]string),
	}
	_ = h.Load()
	return h
}

func resolvePath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (h *History) writeContentBackupLocked(absPath string, data []byte) (string, string) {
	pathSum := sha256.Sum256([]byte(absPath))
	contentSum := sha256.Sum256(data)
	contentHash := hex.EncodeToString(contentSum[:])
	bp := filepath.Join(h.sessionDir, fmt.Sprintf("%x@%x", pathSum[:8], contentSum[:12]))
	if _, err := os.Stat(bp); err != nil {
		_ = os.MkdirAll(h.sessionDir, 0o755)
		_ = os.WriteFile(bp, data, 0o644)
	}
	return bp, contentHash
}

// BeginTurn records conversation boundary metadata before a turn executes tools.
// If no checkpoints exist yet when the first file edit occurs in this turn, an
// initial pre-edit baseline checkpoint (index 0) is materialized automatically.
func (h *History) BeginTurn(msgIndex int, userText string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.snapshots) > 0 {
		return
	}
	label := strings.TrimSpace(userText)
	if len(label) > 48 {
		label = label[:48] + "..."
	}
	if label == "" {
		label = "Initial workspace state"
	} else {
		label = "Initial state (before: " + label + ")"
	}
	h.pendingBaseline = &pendingTurn{
		msgIndex: msgIndex,
		userText: label,
	}
}

// TrackEdit backs up the file at path before it gets modified. Call this before
// any write/edit operation. If the file does not exist yet, its non-existence is
// recorded in Origins so Rewind removes the agent-created file when rolling back.
func (h *History) TrackEdit(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	absPath := resolvePath(path)

	if _, known := h.origins[absPath]; !known {
		if data, err := os.ReadFile(absPath); err == nil {
			bp, hash := h.writeContentBackupLocked(absPath, data)
			h.origins[absPath] = FileOrigin{
				Existed:     true,
				BackupPath:  bp,
				ContentHash: hash,
			}
			if h.lastAgentHash[absPath] == "" {
				h.lastAgentHash[absPath] = hash
			}
		} else {
			h.origins[absPath] = FileOrigin{Existed: false}
		}
	}

	if len(h.snapshots) == 0 && h.pendingBaseline != nil {
		now := time.Now()
		baseBackups := make(map[string]Backup)
		if orig, ok := h.origins[absPath]; ok {
			baseBackups[absPath] = Backup{
				BackupPath:  orig.BackupPath,
				Version:     0,
				ContentHash: orig.ContentHash,
				Existed:     orig.Existed,
				Time:        now,
			}
		}
		h.snapshots = append(h.snapshots, Snapshot{
			MessageIndex: h.pendingBaseline.msgIndex,
			UserText:     h.pendingBaseline.userText,
			Backups:      baseBackups,
			Timestamp:    now,
		})
		h.pendingBaseline = nil
	}

	newVer := h.trackedFiles[absPath] + 1
	h.trackedFiles[absPath] = newVer

	if data, err := os.ReadFile(absPath); err == nil {
		_, _ = h.writeContentBackupLocked(absPath, data)
	}

	_ = h.saveLocked()
}

// RecordWrite records the content written by an agent tool immediately after modification,
// updating LastAgentHash so subsequent external user edits can be distinguished from agent edits.
func (h *History) RecordWrite(path string, content []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()

	absPath := resolvePath(path)
	if _, known := h.origins[absPath]; !known {
		h.origins[absPath] = FileOrigin{Existed: false}
	}
	if h.trackedFiles[absPath] == 0 {
		h.trackedFiles[absPath] = 1
	}
	_, hash := h.writeContentBackupLocked(absPath, content)
	h.lastAgentHash[absPath] = hash
	_ = h.saveLocked()
}

// MakeSnapshot creates and persists a checkpoint associated with msgIndex.
func (h *History) MakeSnapshot(msgIndex int, userText string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.pendingBaseline = nil
	now := time.Now()
	backups := make(map[string]Backup, len(h.trackedFiles))

	for path, ver := range h.trackedFiles {
		data, err := os.ReadFile(path)
		if err == nil {
			bp, hash := h.writeContentBackupLocked(path, data)
			backups[path] = Backup{
				BackupPath:  bp,
				Version:     ver,
				ContentHash: hash,
				Existed:     true,
				Time:        now,
			}
			h.lastAgentHash[path] = hash
		} else {
			backups[path] = Backup{
				Version: ver,
				Existed: false,
				Time:    now,
			}
			delete(h.lastAgentHash, path)
		}
	}

	snap := Snapshot{
		MessageIndex: msgIndex,
		UserText:     userText,
		Backups:      backups,
		Timestamp:    now,
	}

	h.snapshots = append(h.snapshots, snap)
	if len(h.snapshots) > maxSnapshots {
		h.snapshots = h.snapshots[len(h.snapshots)-maxSnapshots:]
	}
	_ = h.saveLocked()
}

// GetSnapshots returns a deep copy of all snapshots for UI inspection.
func (h *History) GetSnapshots() []Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Snapshot, len(h.snapshots))
	for i, s := range h.snapshots {
		cp := make(map[string]Backup, len(s.Backups))
		for k, v := range s.Backups {
			cp[k] = v
		}
		s.Backups = cp
		out[i] = s
	}
	return out
}

// HasSnapshots returns true if at least one checkpoint is available.
func (h *History) HasSnapshots() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.snapshots) > 0
}

// Rewind restores files to snapshotIndex while refusing to overwrite external user edits.
// Returns the sorted list of restored and deleted file paths.
func (h *History) Rewind(snapshotIndex int) ([]string, error) {
	res, err := h.RewindSafe(snapshotIndex, false)
	if err != nil {
		return nil, err
	}
	return res.ChangedFiles(), nil
}

// RewindSafe restores tracked files to snapshotIndex and deletes files created by the agent
// after snapshotIndex. When force is false, any file whose current disk SHA-256 differs from
// both LastAgentHash and the target checkpoint hash is preserved untouched and reported in
// RewindResult.SkippedUserModified.
func (h *History) RewindSafe(snapshotIndex int, force bool) (*RewindResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if snapshotIndex < 0 || snapshotIndex >= len(h.snapshots) {
		return nil, fmt.Errorf("invalid snapshot index %d", snapshotIndex)
	}

	target := h.snapshots[snapshotIndex]

	pathSet := make(map[string]struct{})
	for p := range h.trackedFiles {
		pathSet[p] = struct{}{}
	}
	for p := range h.origins {
		pathSet[p] = struct{}{}
	}
	for _, snap := range h.snapshots {
		for p := range snap.Backups {
			pathSet[p] = struct{}{}
		}
	}

	allPaths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		allPaths = append(allPaths, p)
	}
	sort.Strings(allPaths)

	res := &RewindResult{}
	skippedSet := make(map[string]bool)

	for _, path := range allPaths {
		var (
			wantExist bool
			wantData  []byte
			wantHash  string
		)

		if b, inTarget := target.Backups[path]; inTarget {
			if b.Existed || (b.BackupPath != "" && b.ContentHash == "") {
				data, err := os.ReadFile(b.BackupPath)
				if err == nil {
					wantExist = true
					wantData = data
					wantHash = b.ContentHash
					if wantHash == "" {
						wantHash = sha256Hex(data)
					}
				} else if b.Existed {
					return nil, fmt.Errorf("read backup for %s: %w", path, err)
				}
			}
		} else if orig, hasOrig := h.origins[path]; hasOrig {
			if orig.Existed {
				data, err := os.ReadFile(orig.BackupPath)
				if err != nil {
					return nil, fmt.Errorf("read baseline backup for %s: %w", path, err)
				}
				wantExist = true
				wantData = data
				wantHash = orig.ContentHash
				if wantHash == "" {
					wantHash = sha256Hex(data)
				}
			}
		}

		curData, curErr := os.ReadFile(path)
		curExists := curErr == nil
		curHash := ""
		if curExists {
			curHash = sha256Hex(curData)
		}

		if wantExist {
			if curExists && curHash == wantHash {
				h.lastAgentHash[path] = wantHash
				continue
			}
			if curExists && !force {
				agentHash := h.lastAgentHash[path]
				if (agentHash != "" && curHash != agentHash) || (agentHash == "" && curHash != wantHash) {
					res.SkippedUserModified = append(res.SkippedUserModified, path)
					skippedSet[path] = true
					continue
				}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, fmt.Errorf("mkdir for %s: %w", path, err)
			}
			if err := os.WriteFile(path, wantData, 0o644); err != nil {
				return nil, fmt.Errorf("restore %s: %w", path, err)
			}
			h.lastAgentHash[path] = wantHash
			res.Restored = append(res.Restored, path)
		} else {
			if !curExists {
				delete(h.lastAgentHash, path)
				continue
			}
			if !force {
				agentHash := h.lastAgentHash[path]
				if agentHash == "" || curHash != agentHash {
					res.SkippedUserModified = append(res.SkippedUserModified, path)
					skippedSet[path] = true
					continue
				}
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("remove agent-created file %s: %w", path, err)
			}
			delete(h.lastAgentHash, path)
			res.Deleted = append(res.Deleted, path)
		}
	}

	sort.Strings(res.Restored)
	sort.Strings(res.Deleted)
	sort.Strings(res.SkippedUserModified)

	h.snapshots = h.snapshots[:snapshotIndex+1]

	// Rebuild trackedFiles from remaining snapshots plus any conflict-skipped paths.
	stillReferenced := make(map[string]int)
	for _, snap := range h.snapshots {
		for p, b := range snap.Backups {
			if b.Version > stillReferenced[p] {
				stillReferenced[p] = b.Version
			}
			if _, exists := stillReferenced[p]; !exists {
				stillReferenced[p] = b.Version
			}
		}
	}
	for p := range skippedSet {
		if ver := h.trackedFiles[p]; ver > stillReferenced[p] {
			stillReferenced[p] = ver
		}
	}

	for p := range h.origins {
		if _, keep := stillReferenced[p]; !keep && !skippedSet[p] {
			delete(h.origins, p)
		}
	}
	h.trackedFiles = stillReferenced

	if err := h.saveLocked(); err != nil {
		return res, err
	}
	return res, nil
}

// Save persists snapshot and file-tracking metadata to snapshots.json.
func (h *History) Save() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.saveLocked()
}

func (h *History) saveLocked() error {
	if err := os.MkdirAll(h.sessionDir, 0o755); err != nil {
		return err
	}
	state := persistedState{
		Snapshots:     h.snapshots,
		TrackedFiles:  h.trackedFiles,
		Origins:       h.origins,
		LastAgentHash: h.lastAgentHash,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	target := filepath.Join(h.sessionDir, snapshotsFileName)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return os.WriteFile(target, data, 0o644)
	}
	return nil
}

// Load restores snapshot and file-tracking metadata from snapshots.json if it exists.
func (h *History) Load() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	target := filepath.Join(h.sessionDir, snapshotsFileName)
	raw, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}

	// Backward compatibility with legacy []Snapshot array format.
	if trimmed[0] == '[' {
		var legacy []Snapshot
		if err := json.Unmarshal(trimmed, &legacy); err != nil {
			return err
		}
		h.snapshots = legacy
		h.trackedFiles = make(map[string]int)
		h.origins = make(map[string]FileOrigin)
		h.lastAgentHash = make(map[string]string)
		for i := range h.snapshots {
			for p, b := range h.snapshots[i].Backups {
				if b.BackupPath != "" && !b.Existed {
					if data, readErr := os.ReadFile(b.BackupPath); readErr == nil {
						b.Existed = true
						b.ContentHash = sha256Hex(data)
						h.snapshots[i].Backups[p] = b
					}
				}
				if b.Version > h.trackedFiles[p] {
					h.trackedFiles[p] = b.Version
				}
				if b.Existed && b.ContentHash != "" {
					h.lastAgentHash[p] = b.ContentHash
				}
			}
		}
		return nil
	}

	var state persistedState
	if err := json.Unmarshal(trimmed, &state); err != nil {
		return err
	}
	h.snapshots = state.Snapshots
	if state.TrackedFiles != nil {
		h.trackedFiles = state.TrackedFiles
	} else {
		h.trackedFiles = make(map[string]int)
	}
	if state.Origins != nil {
		h.origins = state.Origins
	} else {
		h.origins = make(map[string]FileOrigin)
	}
	if state.LastAgentHash != nil {
		h.lastAgentHash = state.LastAgentHash
	} else {
		h.lastAgentHash = make(map[string]string)
	}
	for _, snap := range h.snapshots {
		for p, b := range snap.Backups {
			if b.Version > h.trackedFiles[p] {
				h.trackedFiles[p] = b.Version
			}
		}
	}
	return nil
}
