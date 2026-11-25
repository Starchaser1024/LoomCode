package agent

import (
	"context"
	"sync"

	"github.com/Starchaser1024/LoomCode/internal/llm"
	"github.com/Starchaser1024/LoomCode/internal/permissions"
	"github.com/Starchaser1024/LoomCode/internal/tools"
)

// StreamingExecutor dispatches tool calls as soon as their JSON arguments finish
// streaming from the model. Read-only tools (CategoryRead) execute concurrently
// with sibling reads, while mutating or shell tools (CategoryWrite, CategoryCommand)
// act as ordering barriers so sequential edits and post-edit test runs never race.
type StreamingExecutor struct {
	registry *tools.Registry
	checker  *permissions.Checker
	eventCh  chan AgentEvent

	mu      sync.Mutex
	pending []pendingTool
	wg      sync.WaitGroup
}

type pendingTool struct {
	call   llm.ToolCallComplete
	isRead bool
	doneCh chan struct{}
	result toolExecResult
	done   bool
}

func NewStreamingExecutor(registry *tools.Registry, checker *permissions.Checker, eventCh chan AgentEvent) *StreamingExecutor {
	return &StreamingExecutor{
		registry: registry,
		checker:  checker,
		eventCh:  eventCh,
	}
}

func (se *StreamingExecutor) Submit(ctx context.Context, agent *Agent, tc llm.ToolCallComplete) {
	isRead := false
	if se.registry != nil {
		if t := se.registry.Get(tc.ToolName); t != nil && t.Category() == tools.CategoryRead {
			isRead = true
		}
	}

	doneCh := make(chan struct{})

	se.mu.Lock()
	idx := len(se.pending)
	var waitFor []chan struct{}
	if isRead {
		// Wait only for the latest preceding write/command barrier so sibling reads run in parallel.
		for i := idx - 1; i >= 0; i-- {
			if !se.pending[i].isRead {
				waitFor = append(waitFor, se.pending[i].doneCh)
				break
			}
		}
	} else {
		// Write/command barrier: wait for every previously submitted tool in this turn.
		for i := 0; i < idx; i++ {
			waitFor = append(waitFor, se.pending[i].doneCh)
		}
	}
	se.pending = append(se.pending, pendingTool{
		call:   tc,
		isRead: isRead,
		doneCh: doneCh,
	})
	se.mu.Unlock()

	se.wg.Add(1)
	go func() {
		defer se.wg.Done()
		defer close(doneCh)

		for _, dep := range waitFor {
			<-dep
		}

		result := agent.executeSingleTool(ctx, se.eventCh, tc)

		se.mu.Lock()
		se.pending[idx].result = result
		se.pending[idx].done = true
		se.mu.Unlock()
	}()
}

func (se *StreamingExecutor) CollectResults() []toolExecResult {
	se.wg.Wait()

	se.mu.Lock()
	defer se.mu.Unlock()

	results := make([]toolExecResult, len(se.pending))
	for i, p := range se.pending {
		results[i] = p.result
	}
	return results
}

func (se *StreamingExecutor) HasPending() bool {
	se.mu.Lock()
	defer se.mu.Unlock()
	return len(se.pending) > 0
}

func (se *StreamingExecutor) Reset() {
	se.mu.Lock()
	defer se.mu.Unlock()
	se.pending = nil
}
