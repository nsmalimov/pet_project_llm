// Package executor abstracts "something that can run an agent step".
// Implementations: Claude Code CLI, a scripted mock for tests/offline runs,
// and a plain command runner used by the tester role. The core domain and
// engine depend only on the interface, never on Claude specifics.
package executor

import (
	"context"
	"time"
)

// Request describes one agent invocation.
type Request struct {
	Role    string // researcher | developer | reviewer | ...
	Prompt  string
	WorkDir string // directory the agent operates in (task worktree root)
	Model   string // executor-specific model hint, may be empty

	// ReadOnly asks the executor to prevent file modifications (best effort;
	// enforced via tool allow-lists for Claude CLI).
	ReadOnly bool
	Timeout  time.Duration

	// Attempt lets scripted executors vary responses across retries.
	Attempt int
	// Scenario selects a scripted reply set (Local Pilot examples).
	Scenario string
}

// Result is the structured outcome of an agent invocation.
type Result struct {
	Output string // the agent's final text (roles parse a trailing JSON block)

	// Observability. Zero values mean "unknown" for non-LLM executors.
	InputTokens  int
	OutputTokens int
	NumTurns     int
	CostUSD      float64
	DurationMS   int64
	SessionID    string
}

type Executor interface {
	Name() string
	Run(ctx context.Context, req Request) (Result, error)
}
