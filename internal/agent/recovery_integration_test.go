package agent

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Starchaser1024/LoomCode/internal/conversation"
	"github.com/Starchaser1024/LoomCode/internal/filehistory"
	"github.com/Starchaser1024/LoomCode/internal/llm"
	"github.com/Starchaser1024/LoomCode/internal/permissions"
	"github.com/Starchaser1024/LoomCode/internal/session"
)

// TestBoundedSmallFixesSuite exercises 6 concrete small-fix tasks end-to-end through Agent.Run,
// ReadFile/EditFile/Bash tools, real `go test ./...` verification, AST checks, and receipt auditing.
func TestBoundedSmallFixesSuite(t *testing.T) {
	t.Run("01_off_by_one_pagination", func(t *testing.T) {
		t.Parallel()
		fx := newGoFixtureRepo(t, "fix-01", map[string]string{
			"pager.go": `package pager

func Page[T any](items []T, offset, limit int) []T {
	if limit <= 0 || offset < 0 || offset >= len(items)-1 {
		return nil
	}
	end := offset + limit
	if end > len(items)-1 {
		end = len(items) - 1
	}
	return items[offset:end]
}
`,
			"pager_test.go": `package pager

import (
	"reflect"
	"testing"
)

func TestPageBounds(t *testing.T) {
	items := []int{10, 20, 30, 40, 50}
	if got := Page(items, 3, 5); !reflect.DeepEqual(got, []int{40, 50}) {
		t.Fatalf("tail page = %v, want [40 50]", got)
	}
	if got := Page(items, 4, 1); !reflect.DeepEqual(got, []int{50}) {
		t.Fatalf("last element = %v, want [50]", got)
	}
	if got := Page(items, 5, 2); got != nil {
		t.Fatalf("out-of-range offset = %v, want nil", got)
	}
}
`,
		})
		fx.assertBaselineFails()
		src := fx.path("pager.go")

		client := &turnScriptClient{handlers: []func(int, []conversation.Message) stepOutcome{
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": src}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 90, OutputTokens: 25}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "e1", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  src,
						"old_string": "if limit <= 0 || offset < 0 || offset >= len(items)-1 {\n\t\treturn nil\n\t}\n\tend := offset + limit\n\tif end > len(items)-1 {\n\t\tend = len(items) - 1\n\t}",
						"new_string": "if limit <= 0 || offset < 0 || offset >= len(items) {\n\t\treturn nil\n\t}\n\tend := offset + limit\n\tif end > len(items) {\n\t\tend = len(items)\n\t}",
					}},
					llm.ToolCallComplete{ToolID: "b1", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 140, OutputTokens: 55}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.TextDelta{Text: "Fixed off-by-one bounds in Page."},
					llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 80, OutputTokens: 15}},
				}}
			},
		}}

		ag := fx.newAgent(client)
		conv := conversation.NewManager()
		runConversationRound(ag, conv, "Fix the off-by-one pagination bug in pager.go")

		fx.assertTestsPassAndUntampered()
		assertGoFileDeclaresSymbol(t, src, "Page")
		rec := ag.GetReceipt()
		if len(rec.TouchedFiles) != 1 || rec.TouchedFiles[0].Action != FileModified || len(rec.Commands) != 1 || !rec.Commands[0].Passed {
			t.Fatalf("unexpected receipt: %+v", rec)
		}
	})

	t.Run("02_missing_input_validation", func(t *testing.T) {
		t.Parallel()
		fx := newGoFixtureRepo(t, "fix-02", map[string]string{
			"user.go": `package user

import "fmt"

type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Reason)
}

type Account struct {
	Email string
	Age   int
}

func NewAccount(email string, age int) (*Account, error) {
	return &Account{Email: email, Age: age}, nil
}
`,
			"user_test.go": `package user

import (
	"errors"
	"testing"
)

func TestNewAccountValidation(t *testing.T) {
	var ve *ValidationError
	if _, err := NewAccount("bad-email", 25); !errors.As(err, &ve) || ve.Field != "email" {
		t.Fatalf("expected ValidationError on email, got %v", err)
	}
	if _, err := NewAccount("alice@example.com", -1); !errors.As(err, &ve) || ve.Field != "age" {
		t.Fatalf("expected ValidationError on age, got %v", err)
	}
	acc, err := NewAccount("alice@example.com", 30)
	if err != nil || acc.Email != "alice@example.com" {
		t.Fatalf("unexpected valid account result: %+v, err=%v", acc, err)
	}
}
`,
		})
		fx.assertBaselineFails()
		src := fx.path("user.go")

		client := &turnScriptClient{handlers: []func(int, []conversation.Message) stepOutcome{
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": src}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 100, OutputTokens: 20}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "e1", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  src,
						"old_string": "import \"fmt\"",
						"new_string": "import (\n\t\"fmt\"\n\t\"strings\"\n)",
					}},
					llm.ToolCallComplete{ToolID: "e2", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  src,
						"old_string": "func NewAccount(email string, age int) (*Account, error) {\n\treturn &Account{Email: email, Age: age}, nil\n}",
						"new_string": "func NewAccount(email string, age int) (*Account, error) {\n\temail = strings.TrimSpace(email)\n\tif email == \"\" || !strings.Contains(email, \"@\") {\n\t\treturn nil, &ValidationError{Field: \"email\", Reason: \"must contain @\"}\n\t}\n\tif age < 0 || age > 150 {\n\t\treturn nil, &ValidationError{Field: \"age\", Reason: \"out of range\"}\n\t}\n\treturn &Account{Email: email, Age: age}, nil\n}",
					}},
					llm.ToolCallComplete{ToolID: "b1", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 160, OutputTokens: 70}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.TextDelta{Text: "Added structured ValidationError checks in NewAccount."},
					llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 90, OutputTokens: 18}},
				}}
			},
		}}

		ag := fx.newAgent(client)
		conv := conversation.NewManager()
		runConversationRound(ag, conv, "Validate email and age in NewAccount")

		fx.assertTestsPassAndUntampered()
		assertGoFileDeclaresSymbol(t, src, "ValidationError")
	})

	t.Run("03_config_empty_value_parser", func(t *testing.T) {
		t.Parallel()
		fx := newGoFixtureRepo(t, "fix-03", map[string]string{
			"kvconf.go": `package kvconf

import (
	"fmt"
	"strings"
)

func Parse(raw string) (map[string]string, error) {
	out := make(map[string]string)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "=")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("malformed entry: %q", line)
		}
		out[parts[0]] = parts[1]
	}
	return out, nil
}
`,
			"kvconf_test.go": `package kvconf

import "testing"

func TestParseEdgeCases(t *testing.T) {
	cfg, err := Parse("# comment\nEMPTY_VAL=\nDSN=postgres://localhost:5432/db?sslmode=disable\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, ok := cfg["EMPTY_VAL"]; !ok || v != "" {
		t.Fatalf("EMPTY_VAL = %q (ok=%v), want empty string", v, ok)
	}
	if cfg["DSN"] != "postgres://localhost:5432/db?sslmode=disable" {
		t.Fatalf("DSN = %q", cfg["DSN"])
	}
	if _, err := Parse("=missing_key"); err == nil {
		t.Fatal("expected error for empty key")
	}
}
`,
		})
		fx.assertBaselineFails()
		src := fx.path("kvconf.go")

		client := &turnScriptClient{handlers: []func(int, []conversation.Message) stepOutcome{
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": src}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 95, OutputTokens: 20}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "e1", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  src,
						"old_string": "\t\tparts := strings.Split(line, \"=\")\n\t\tif len(parts) != 2 || parts[0] == \"\" || parts[1] == \"\" {\n\t\t\treturn nil, fmt.Errorf(\"malformed entry: %q\", line)\n\t\t}\n\t\tout[parts[0]] = parts[1]",
						"new_string": "\t\tkey, val, ok := strings.Cut(line, \"=\")\n\t\tkey = strings.TrimSpace(key)\n\t\tif !ok || key == \"\" {\n\t\t\treturn nil, fmt.Errorf(\"malformed entry: %q\", line)\n\t\t}\n\t\tout[key] = strings.TrimSpace(val)",
					}},
					llm.ToolCallComplete{ToolID: "b1", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 150, OutputTokens: 60}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.TextDelta{Text: "Updated Parse to use strings.Cut and allow empty values."},
					llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 80, OutputTokens: 16}},
				}}
			},
		}}

		ag := fx.newAgent(client)
		conv := conversation.NewManager()
		runConversationRound(ag, conv, "Fix empty value and '=' handling in kvconf.Parse")

		fx.assertTestsPassAndUntampered()
		data, _ := os.ReadFile(src)
		if !strings.Contains(string(data), "strings.Cut") {
			t.Fatalf("expected strings.Cut in %s", src)
		}
	})

	t.Run("04_http_handler_status_and_headers", func(t *testing.T) {
		t.Parallel()
		fx := newGoFixtureRepo(t, "fix-04", map[string]string{
			"health.go": `package health

import (
	"encoding/json"
	"net/http"
)

func Handler(ready func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok := ready()
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": ok})
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}
}
`,
			"health_test.go": `package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthHandlerContract(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(func() bool { return false })(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("degraded status = %d, want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}
`,
		})
		fx.assertBaselineFails()
		src := fx.path("health.go")

		client := &turnScriptClient{handlers: []func(int, []conversation.Message) stepOutcome{
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": src}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 90, OutputTokens: 20}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "e1", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  src,
						"old_string": "\t\tok := ready()\n\t\t_ = json.NewEncoder(w).Encode(map[string]bool{\"ok\": ok})\n\t\tif !ok {\n\t\t\tw.WriteHeader(http.StatusServiceUnavailable)\n\t\t}",
						"new_string": "\t\tok := ready()\n\t\tw.Header().Set(\"Content-Type\", \"application/json; charset=utf-8\")\n\t\tw.Header().Set(\"Cache-Control\", \"no-store\")\n\t\tif !ok {\n\t\t\tw.WriteHeader(http.StatusServiceUnavailable)\n\t\t} else {\n\t\t\tw.WriteHeader(http.StatusOK)\n\t\t}\n\t\t_ = json.NewEncoder(w).Encode(map[string]bool{\"ok\": ok})",
					}},
					llm.ToolCallComplete{ToolID: "b1", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 145, OutputTokens: 65}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.TextDelta{Text: "Fixed response headers and 503 status ordering."},
					llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 75, OutputTokens: 14}},
				}}
			},
		}}

		ag := fx.newAgent(client)
		conv := conversation.NewManager()
		runConversationRound(ag, conv, "Fix 503 status code and JSON/no-store headers in Handler")

		fx.assertTestsPassAndUntampered()
	})

	t.Run("05_nil_map_concurrent_init", func(t *testing.T) {
		t.Parallel()
		fx := newGoFixtureRepo(t, "fix-05", map[string]string{
			"registry.go": `package registry

type Registry struct {
	entries map[string]string
}

func New() *Registry {
	return &Registry{}
}

func (r *Registry) Set(k, v string) {
	r.entries[k] = v
}

func (r *Registry) Get(k string) (string, bool) {
	v, ok := r.entries[k]
	return v, ok
}
`,
			"registry_test.go": `package registry

import (
	"fmt"
	"sync"
	"testing"
)

func TestRegistryConcurrentInitAndAccess(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("k-%d", id)
			r.Set(key, "val")
			_, _ = r.Get(key)
		}(i)
	}
	wg.Wait()
	if v, ok := r.Get("k-0"); !ok || v != "val" {
		t.Fatalf("Get(k-0) = %q, %v", v, ok)
	}
}
`,
		})
		fx.assertBaselineFails()
		src := fx.path("registry.go")

		client := &turnScriptClient{handlers: []func(int, []conversation.Message) stepOutcome{
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": src}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 85, OutputTokens: 20}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "w1", ToolName: "WriteFile", Arguments: map[string]any{
						"file_path": src,
						"content": `package registry

import "sync"

type Registry struct {
	mu      sync.RWMutex
	entries map[string]string
}

func New() *Registry {
	return &Registry{entries: make(map[string]string)}
}

func (r *Registry) Set(k, v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]string)
	}
	r.entries[k] = v
}

func (r *Registry) Get(k string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.entries[k]
	return v, ok
}
`,
					}},
					llm.ToolCallComplete{ToolID: "b1", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 155, OutputTokens: 80}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.TextDelta{Text: "Initialized map in New and added RWMutex synchronization."},
					llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 70, OutputTokens: 15}},
				}}
			},
		}}

		ag := fx.newAgent(client)
		conv := conversation.NewManager()
		runConversationRound(ag, conv, "Fix nil map panic and concurrent access in Registry")

		fx.assertTestsPassAndUntampered()
		assertGoFileDeclaresSymbol(t, src, "New")
	})

	t.Run("06_multifile_interface_rename", func(t *testing.T) {
		t.Parallel()
		fx := newGoFixtureRepo(t, "fix-06", map[string]string{
			"store.go": `package store

import "context"

type KVStore interface {
	Get(ctx context.Context, key string) (string, bool)
}
`,
			"mem_store.go": `package store

import "context"

type MemStore struct {
	Data map[string]string
}

func (m *MemStore) Fetch(_ context.Context, key string) (string, bool) {
	v, ok := m.Data[key]
	return v, ok
}
`,
			"service.go": `package store

import "context"

type LookupService struct {
	Store KVStore
}

func (s *LookupService) Resolve(ctx context.Context, key string) string {
	if v, ok := s.Store.Fetch(ctx, key); ok {
		return v
	}
	return "default"
}
`,
			"service_test.go": `package store

import (
	"context"
	"testing"
)

func TestLookupService(t *testing.T) {
	svc := &LookupService{Store: &MemStore{Data: map[string]string{"env": "prod"}}}
	if got := svc.Resolve(context.Background(), "env"); got != "prod" {
		t.Fatalf("Resolve(env) = %q, want prod", got)
	}
}
`,
		})
		fx.assertBaselineFails()
		memPath := fx.path("mem_store.go")
		svcPath := fx.path("service.go")

		client := &turnScriptClient{handlers: []func(int, []conversation.Message) stepOutcome{
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": memPath}},
					llm.ToolCallComplete{ToolID: "r2", ToolName: "ReadFile", Arguments: map[string]any{"file_path": svcPath}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 120, OutputTokens: 30}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "e1", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  memPath,
						"old_string": "func (m *MemStore) Fetch(_ context.Context, key string) (string, bool)",
						"new_string": "func (m *MemStore) Get(_ context.Context, key string) (string, bool)",
					}},
					llm.ToolCallComplete{ToolID: "e2", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  svcPath,
						"old_string": "s.Store.Fetch(ctx, key)",
						"new_string": "s.Store.Get(ctx, key)",
					}},
					llm.ToolCallComplete{ToolID: "b1", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 170, OutputTokens: 65}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.TextDelta{Text: "Aligned MemStore and LookupService with KVStore.Get."},
					llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 85, OutputTokens: 15}},
				}}
			},
		}}

		ag := fx.newAgent(client)
		conv := conversation.NewManager()
		runConversationRound(ag, conv, "Align MemStore and LookupService with KVStore.Get")

		fx.assertTestsPassAndUntampered()
		rec := ag.GetReceipt()
		if len(rec.TouchedFiles) != 2 {
			t.Fatalf("expected 2 touched files in receipt, got %+v", rec.TouchedFiles)
		}
	})
}

// TestFailureRecoveryAndSafetySuite exercises 4 concrete failure-recovery scenarios:
// (07) transient 429 + mid-stream 502 retry without duplicate edits,
// (08) mid-task context cancellation + JSONL session & filehistory reload /resume,
// (09) failing `go test` inspection & self-repair,
// (10) multi-turn bad refactor `/rewind` deleting created files while preserving user manual edits.
func TestFailureRecoveryAndSafetySuite(t *testing.T) {
	t.Run("07_transient_429_and_midstream_502_no_duplicate_edit", func(t *testing.T) {
		t.Parallel()
		fx := newGoFixtureRepo(t, "rec-07", map[string]string{
			"counter.go": `package counter

func Step(cur, delta int) int {
	return cur - delta
}
`,
			"counter_test.go": `package counter

import "testing"

func TestStep(t *testing.T) {
	if got := Step(10, 5); got != 15 {
		t.Fatalf("Step(10, 5) = %d, want 15", got)
	}
}
`,
		})
		fx.assertBaselineFails()
		src := fx.path("counter.go")
		var editCalls int32

		client := &turnScriptClient{handlers: []func(int, []conversation.Message) stepOutcome{
			// Call 0: upstream 429 rate limit before any output.
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{err: &llm.RateLimitError{Message: "Rate limited", RetryAfter: "0"}}
			},
			// Call 1: reads file cleanly.
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": src}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 80, OutputTokens: 20}},
				}}
			},
			// Call 2: emits EditFile ToolCallComplete (which executes concurrently!) and THEN drops with 502!
			func(_ int, _ []conversation.Message) stepOutcome {
				atomic.AddInt32(&editCalls, 1)
				return stepOutcome{
					events: []llm.StreamEvent{
						llm.ToolCallComplete{ToolID: "e1", ToolName: "EditFile", Arguments: map[string]any{
							"file_path":  src,
							"old_string": "return cur - delta",
							"new_string": "return cur + delta",
						}},
					},
					err: &llm.LLMError{StatusCode: 502, Message: "API error (502): upstream gateway reset"},
				}
			},
			// Call 3 (retry after 502): inspects conv — EditFile tool_result MUST already be present, so
			// the model does NOT re-run EditFile and proceeds directly to `go test ./...`!
			func(_ int, msgs []conversation.Message) stepOutcome {
				lastOut := lastToolResultText(msgs)
				if !strings.Contains(lastOut, "Successfully edited") {
					t.Errorf("expected completed EditFile tool_result in conversation before retry, got %q", lastOut)
				}
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "b1", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 130, OutputTokens: 30}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.TextDelta{Text: "Recovered from transient errors and verified Step."},
					llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 90, OutputTokens: 18}},
				}}
			},
		}}

		ag := fx.newAgent(client)
		conv := conversation.NewManager()
		runConversationRound(ag, conv, "Fix Step in counter.go")

		if got := atomic.LoadInt32(&editCalls); got != 1 {
			t.Fatalf("expected EditFile to be issued exactly once, got %d", got)
		}
		fx.assertTestsPassAndUntampered()

		rec := ag.GetReceipt()
		recoveredRetries := 0
		for _, r := range rec.FailuresRecoveries {
			if r.Kind == "api_retry" && r.Outcome == "recovered" {
				recoveredRetries++
			}
		}
		if recoveredRetries != 2 {
			t.Fatalf("expected 2 recovered api_retry records (429 + 502), got %+v", rec.FailuresRecoveries)
		}
	})

	t.Run("08_context_interrupt_and_session_resume", func(t *testing.T) {
		t.Parallel()
		fx := newGoFixtureRepo(t, "rec-08-session", map[string]string{
			"slug.go": `package slug

import "strings"

func Normalize(s string) string {
	return strings.TrimSpace(s)
}
`,
			"slug_test.go": `package slug

import "testing"

func TestNormalize(t *testing.T) {
	if got := Normalize("  Hello World  "); got != "hello-world" {
		t.Fatalf("Normalize = %q, want hello-world", got)
	}
}
`,
		})
		fx.assertBaselineFails()
		src := fx.path("slug.go")

		ctx1, cancel1 := context.WithCancel(context.Background())
		defer cancel1()

		// Process 1: reads and patches slug.go, then gets interrupted (context.Canceled) before `go test`.
		client1 := &turnScriptClient{handlers: []func(int, []conversation.Message) stepOutcome{
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": src}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 70, OutputTokens: 20}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "e1", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  src,
						"old_string": "return strings.TrimSpace(s)",
						"new_string": "return strings.Join(strings.Fields(strings.ToLower(s)), \"-\")",
					}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 110, OutputTokens: 45}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				cancel1()
				return stepOutcome{err: context.Canceled}
			},
		}}

		userPrompt := "Fix slug.Normalize to lowercase words joined by hyphens"
		session.SaveMessage(fx.dir, fx.sessionID, session.Message{
			Role: "user", Content: userPrompt, Ts: time.Now().Unix(),
		})
		ag1 := fx.newAgent(client1)
		conv1 := conversation.NewManager()
		conv1.AddUserMessage(userPrompt)
		_ = collectEvents(ag1.Run(ctx1, conv1))

		session.SaveMessage(fx.dir, fx.sessionID, session.Message{
			Role: "assistant", Content: "(interrupted after patching slug.go)", Ts: time.Now().Unix(),
		})

		rec1 := ag1.GetReceipt()
		hasInterrupt := false
		for _, r := range rec1.FailuresRecoveries {
			if r.Kind == "stream_interrupt" {
				hasInterrupt = true
			}
		}
		if !hasInterrupt {
			t.Fatalf("expected stream_interrupt in Run 1 receipt, got %+v", rec1.FailuresRecoveries)
		}

		// Process 2 (/resume): reload JSONL session + reload filehistory from disk in a fresh Agent.
		loadedMsgs := session.LoadSession(fx.dir, fx.sessionID)
		if len(loadedMsgs) < 2 {
			t.Fatalf("expected persisted session messages, got %d", len(loadedMsgs))
		}
		resumedConv := conversation.NewManager()
		for _, m := range loadedMsgs {
			if m.Role == "user" {
				resumedConv.AddUserMessage(m.Content)
			} else if m.Role == "assistant" {
				resumedConv.AddAssistantMessage(m.Content)
			}
		}

		reloadedHist := filehistory.New(fx.dir, fx.sessionID)
		if !reloadedHist.HasSnapshots() {
			t.Fatal("expected reloaded filehistory to preserve pre-crash baseline checkpoint")
		}
		dt2 := newScopedDefaultTools(fx.dir)
		dt2.EditFile.FileHistory = reloadedHist
		dt2.WriteFile.FileHistory = reloadedHist

		client2 := &turnScriptClient{handlers: []func(int, []conversation.Message) stepOutcome{
			// Resumed agent inspects slug.go, sees patch is ALREADY present, and runs `go test ./...` without re-editing.
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "r2", ToolName: "ReadFile", Arguments: map[string]any{"file_path": src}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 95, OutputTokens: 20}},
				}}
			},
			func(_ int, msgs []conversation.Message) stepOutcome {
				readContent := lastToolResultText(msgs)
				if !strings.Contains(readContent, "strings.Fields(strings.ToLower(s))") {
					t.Errorf("expected resumed ReadFile to observe already-applied patch, got %q", readContent)
				}
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "b2", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 120, OutputTokens: 25}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.TextDelta{Text: "Verified existing patch after resume; all tests pass."},
					llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 80, OutputTokens: 16}},
				}}
			},
		}}

		ag2 := New(client2, dt2.Registry, "anthropic")
		ag2.WorkDir = fx.dir
		ag2.SessionID = fx.sessionID
		ag2.FileHistory = reloadedHist
		ag2.Checker = permissions.NewChecker(permissions.NewStrictPathSandbox(fx.dir), nil, permissions.ModeBypass)

		runConversationRound(ag2, resumedConv, "Continue and verify remaining tests")
		fx.assertTestsPassAndUntampered()
		if len(ag2.GetReceipt().TouchedFiles) != 0 {
			t.Fatalf("expected resumed run not to duplicate already-applied file edits, got %+v", ag2.GetReceipt().TouchedFiles)
		}
	})

	t.Run("09_test_failure_self_repair", func(t *testing.T) {
		t.Parallel()
		fx := newGoFixtureRepo(t, "rec-09", map[string]string{
			"clamp.go": `package clamp

func Int(v, lo, hi int) int {
	return v
}
`,
			"clamp_test.go": `package clamp

import "testing"

func TestInt(t *testing.T) {
	if got := Int(-3, 0, 10); got != 0 {
		t.Fatalf("Int(-3, 0, 10) = %d, want 0", got)
	}
	if got := Int(25, 0, 10); got != 10 {
		t.Fatalf("Int(25, 0, 10) = %d, want 10", got)
	}
	if got := Int(5, 0, 10); got != 5 {
		t.Fatalf("Int(5, 0, 10) = %d, want 5", got)
	}
}
`,
		})
		fx.assertBaselineFails()
		src := fx.path("clamp.go")

		client := &turnScriptClient{handlers: []func(int, []conversation.Message) stepOutcome{
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": src}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 70, OutputTokens: 15}},
				}}
			},
			// First attempt contains a bug (`return lo` on upper bound) -> real `go test ./...` fails!
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "e1", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  src,
						"old_string": "return v",
						"new_string": "if v < lo {\n\t\treturn lo\n\t}\n\tif v > hi {\n\t\treturn lo\n\t}\n\treturn v",
					}},
					llm.ToolCallComplete{ToolID: "b1", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 130, OutputTokens: 50}},
				}}
			},
			// Agent inspects real `go test` failure output ("Int(25, 0, 10) = 0, want 10") and repairs upper bound!
			func(_ int, msgs []conversation.Message) stepOutcome {
				failOut := lastToolResultText(msgs)
				if !strings.Contains(failOut, "want 10") {
					t.Errorf("expected real failing go test output in tool result, got %q", failOut)
				}
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "e2", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  src,
						"old_string": "if v > hi {\n\t\treturn lo\n\t}",
						"new_string": "if v > hi {\n\t\treturn hi\n\t}",
					}},
					llm.ToolCallComplete{ToolID: "b2", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 165, OutputTokens: 45}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.TextDelta{Text: "Repaired upper-bound return value in clamp.Int after test failure."},
					llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 90, OutputTokens: 16}},
				}}
			},
		}}

		ag := fx.newAgent(client)
		conv := conversation.NewManager()
		runConversationRound(ag, conv, "Implement bounds clamping in clamp.Int")

		fx.assertTestsPassAndUntampered()
		rec := ag.GetReceipt()
		if len(rec.Commands) != 2 || rec.Commands[0].Passed || !rec.Commands[1].Passed {
			t.Fatalf("expected [FAIL, PASS] command history, got %+v", rec.Commands)
		}
		repaired := false
		for _, r := range rec.FailuresRecoveries {
			if r.Kind == "test_self_repair" && r.Outcome == "recovered" {
				repaired = true
			}
		}
		if !repaired {
			t.Fatalf("expected recovered test_self_repair in receipt, got %+v", rec.FailuresRecoveries)
		}
	})

	t.Run("10_safe_rewind_rolls_back_bad_refactor_and_preserves_user_edits", func(t *testing.T) {
		t.Parallel()
		fx := newGoFixtureRepo(t, "rec-10-rewind", map[string]string{
			"billing.go": `package billing

func Subtotal(cents []int) int {
	total := 0
	for _, c := range cents {
		if c > 0 {
			total += c
		}
	}
	return total
}
`,
			"notes.go": `package billing

const TaxPolicyNote = "v0-original"
`,
			"billing_test.go": `package billing

import "testing"

func TestApplyDiscount(t *testing.T) {
	if got := ApplyDiscount(1000, 15); got != 850 {
		t.Fatalf("ApplyDiscount(1000, 15) = %d, want 850", got)
	}
}
`,
		})
		fx.assertBaselineFails()
		billingPath := fx.path("billing.go")
		notesPath := fx.path("notes.go")
		badHelperPath := fx.path("bad_refactor_helper.go")

		client := &turnScriptClient{handlers: []func(int, []conversation.Message) stepOutcome{
			// Turn 1 (good turn): implements working ApplyDiscount and verifies `go test ./...`.
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "r1", ToolName: "ReadFile", Arguments: map[string]any{"file_path": billingPath}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 80, OutputTokens: 20}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "e1", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  billingPath,
						"old_string": "\treturn total\n}",
						"new_string": "\treturn total\n}\n\nfunc ApplyDiscount(cents, pct int) int {\n\treturn cents - (cents*pct)/100\n}",
					}},
					llm.ToolCallComplete{ToolID: "b1", ToolName: "Bash", Arguments: map[string]any{"command": "go test ./..."}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 120, OutputTokens: 45}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.TextDelta{Text: "Implemented ApplyDiscount; tests pass."},
					llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 60, OutputTokens: 12}},
				}}
			},
			// Turn 2 (bad refactor part 1): creates bad_refactor_helper.go, edits notes.go, breaks ApplyDiscount.
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "r2", ToolName: "ReadFile", Arguments: map[string]any{"file_path": notesPath}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 90, OutputTokens: 15}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "w2", ToolName: "WriteFile", Arguments: map[string]any{
						"file_path": badHelperPath,
						"content":   "package billing\n\nfunc brokenScale(v int) int { return v * 999 }\n",
					}},
					llm.ToolCallComplete{ToolID: "e2", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  notesPath,
						"old_string": `"v0-original"`,
						"new_string": `"v1-agent-refactor"`,
					}},
					llm.ToolCallComplete{ToolID: "e3", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  billingPath,
						"old_string": "return cents - (cents*pct)/100",
						"new_string": "return brokenScale(cents)",
					}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 140, OutputTokens: 55}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.TextDelta{Text: "Refactored discount helper (part 1)."},
					llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 70, OutputTokens: 12}},
				}}
			},
			// Turn 3 (bad refactor part 2): further mutates billing.go and bad_refactor_helper.go.
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.ToolCallComplete{ToolID: "e4", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  badHelperPath,
						"old_string": "return v * 999",
						"new_string": "return -v",
					}},
					llm.ToolCallComplete{ToolID: "e5", ToolName: "EditFile", Arguments: map[string]any{
						"file_path":  billingPath,
						"old_string": "return brokenScale(cents)",
						"new_string": "return brokenScale(cents) - pct",
					}},
					llm.StreamEnd{StopReason: "tool_use", Usage: llm.UsageInfo{InputTokens: 150, OutputTokens: 40}},
				}}
			},
			func(_ int, _ []conversation.Message) stepOutcome {
				return stepOutcome{events: []llm.StreamEvent{
					llm.TextDelta{Text: "Completed aggressive refactor (part 2)."},
					llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{InputTokens: 75, OutputTokens: 12}},
				}}
			},
		}}

		ag := fx.newAgent(client)
		conv := conversation.NewManager()

		// Execute Turn 1 (good), Turn 2 (bad refactor 1), Turn 3 (bad refactor 2).
		runConversationRound(ag, conv, "Add ApplyDiscount")
		fx.assertTestsPassAndUntampered()

		runConversationRound(ag, conv, "Refactor into helper part 1")
		runConversationRound(ag, conv, "Refactor part 2")
		fx.assertBaselineFails() // Broken by Turn 2 + Turn 3!

		// User manually edits notes.go externally after Turn 3.
		userCustomNotes := "package billing\n\nconst TaxPolicyNote = \"user-manual-rule-do-not-clobber\"\n"
		if err := os.WriteFile(notesPath, []byte(userCustomNotes), 0o644); err != nil {
			t.Fatal(err)
		}

		// Reload filehistory from disk (verifying persistence) and rewind to Snapshot 1 (end of Turn 1).
		reloaded := filehistory.New(fx.dir, fx.sessionID)
		snaps := reloaded.GetSnapshots()
		if len(snaps) != 4 {
			t.Fatalf("expected 4 checkpoints (initial + 3 turns), got %d", len(snaps))
		}

		res, err := reloaded.RewindSafe(1, false)
		if err != nil {
			t.Fatalf("RewindSafe(1, false) failed: %v", err)
		}

		// 1. billing.go restored to Turn 1's passing implementation.
		fx.assertTestsPassAndUntampered()

		// 2. bad_refactor_helper.go created in Turn 2 is deleted from disk.
		if _, err := os.Stat(badHelperPath); !os.IsNotExist(err) {
			t.Fatalf("expected agent-created %s to be deleted on rewind", badHelperPath)
		}
		if len(res.Deleted) != 1 || res.Deleted[0] != badHelperPath {
			t.Fatalf("expected Deleted=[%s], got %v", badHelperPath, res.Deleted)
		}

		// 3. User's manual edit in notes.go is preserved untouched!
		gotNotes, _ := os.ReadFile(notesPath)
		if string(gotNotes) != userCustomNotes {
			t.Fatalf("user manual edit in notes.go was overwritten: got %q", string(gotNotes))
		}
		if len(res.SkippedUserModified) != 1 || res.SkippedUserModified[0] != notesPath {
			t.Fatalf("expected SkippedUserModified=[%s], got %v", notesPath, res.SkippedUserModified)
		}
	})
}
