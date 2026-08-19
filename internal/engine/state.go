package engine

import "orchestrator/internal/domain"

// FullState is the aggregate view a UI (or the CLI) renders: the task plus
// everything observable about it. Structured data, not parsed logs.
type FullState struct {
	Task       *domain.Task      `json:"task"`
	Runs       []domain.AgentRun `json:"runs"`
	Evidence   []domain.Evidence `json:"evidence"`
	Decisions  []domain.Decision `json:"decisions"`
	Confidence string            `json:"confidence"` // strongest evidence level reached
	Totals     Totals            `json:"totals"`
}

type Totals struct {
	AgentRuns    int     `json:"agent_runs"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	DurationMS   int64   `json:"duration_ms"`
}

func (e *Engine) FullState(taskID string) (*FullState, error) {
	t, err := e.Store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	runs, err := e.Store.Runs(taskID)
	if err != nil {
		return nil, err
	}
	evidence, err := e.Store.EvidenceList(taskID)
	if err != nil {
		return nil, err
	}
	decisions, err := e.Store.Decisions(taskID)
	if err != nil {
		return nil, err
	}
	fs := &FullState{Task: t, Runs: runs, Evidence: evidence, Decisions: decisions}
	best := domain.EvidenceLevel("")
	for _, ev := range evidence {
		if ev.Level.Rank() > best.Rank() {
			best = ev.Level
		}
	}
	if best == "" {
		best = domain.EvidenceAssumed
	}
	fs.Confidence = string(best)
	for _, r := range runs {
		fs.Totals.AgentRuns++
		fs.Totals.InputTokens += r.InputTokens
		fs.Totals.OutputTokens += r.OutputTokens
		fs.Totals.CostUSD += r.CostUSD
		fs.Totals.DurationMS += r.DurationMS
	}
	return fs, nil
}
