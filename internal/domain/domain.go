// Package domain holds the core types of the orchestrator. It has no
// dependencies on storage, executors or HTTP — everything else depends on it.
package domain

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// ---------- Task ----------

type TaskStatus string

const (
	StatusPending          TaskStatus = "pending"
	StatusUnderstanding    TaskStatus = "understanding"
	StatusInvestigating    TaskStatus = "investigating"
	StatusImplementing     TaskStatus = "implementing"
	StatusVerifying        TaskStatus = "verifying"
	StatusReviewing        TaskStatus = "reviewing"
	StatusAwaitingDecision TaskStatus = "awaiting_decision"
	StatusDone             TaskStatus = "done"
	StatusFailed           TaskStatus = "failed"
	StatusInterrupted      TaskStatus = "interrupted"
)

// Terminal reports whether the task will not make further progress on its own.
func (s TaskStatus) Terminal() bool {
	return s == StatusDone || s == StatusFailed
}

// Active reports whether an engine step was in flight for this status.
// Used on restart to detect interrupted tasks.
func (s TaskStatus) Active() bool {
	switch s {
	case StatusUnderstanding, StatusInvestigating, StatusImplementing, StatusVerifying, StatusReviewing:
		return true
	}
	return false
}

// RepoRef points at one git repository taking part in a task.
type RepoRef struct {
	Name string `json:"name"` // short name, unique within the task
	Path string `json:"path"` // absolute path to the original repository
}

// TestResult is the outcome of one verification run.
type TestResult struct {
	Repo       string `json:"repo"`
	Command    string `json:"command"`
	Passed     bool   `json:"passed"`
	ExitCode   int    `json:"exit_code"`
	OutputTail string `json:"output_tail"`
}

// Finding is a single reviewer finding.
type Finding struct {
	Severity string `json:"severity"` // high | medium | low
	File     string `json:"file"`
	Issue    string `json:"issue"`
}

// TaskState is the mutable working state of a task. It is persisted with the
// task so the engine can resume after a restart without any LLM context.
type TaskState struct {
	Steps             int `json:"steps"`              // total engine steps taken (budget guard)
	ImplementAttempts int `json:"implement_attempts"` // developer runs so far
	FixAttempts       int `json:"fix_attempts"`       // implement runs caused by failing tests
	ReviewRounds      int `json:"review_rounds"`
	Investigations    int `json:"investigations"`

	Uncertainty     string   `json:"uncertainty,omitempty"` // from last research: low|medium|high
	ResearchSummary string   `json:"research_summary,omitempty"`
	KeyFiles        []string `json:"key_files,omitempty"`
	InvestigationQ  string   `json:"investigation_question,omitempty"` // what a pending investigation should answer

	// DeveloperSummary is the developer's own account of the change. It is
	// deliberately NOT shown to the reviewer (independent review).
	DeveloperSummary string `json:"developer_summary,omitempty"`
	// AuthorModel is the model that produced the current change; the router
	// uses it to pick a different model for independent review.
	AuthorModel    string       `json:"author_model,omitempty"`
	ChangedFiles   []string     `json:"changed_files,omitempty"`
	LastTests      []TestResult `json:"last_tests,omitempty"`
	ReviewFindings []Finding    `json:"review_findings,omitempty"`

	// Notes accumulates human input: decision resolutions, extra guidance.
	Notes []string `json:"notes,omitempty"`

	// ResumeStatus remembers which phase an interrupted task should return to.
	ResumeStatus TaskStatus `json:"resume_status,omitempty"`

	// WorktreeRoot is the directory that contains one worktree per repo.
	WorktreeRoot string            `json:"worktree_root,omitempty"`
	BaseSHAs     map[string]string `json:"base_shas,omitempty"` // repo name -> sha worktree was created from
	Branch       string            `json:"branch,omitempty"`    // branch used in every worktree
}

// Task is a single engineering task moving through the workflow.
type Task struct {
	ID      string    `json:"id"`
	Goal    string    `json:"goal"`
	Context []string  `json:"context,omitempty"` // extra context sources (freeform)
	Repos   []RepoRef `json:"repos"`
	// TestCommand overrides auto-detected verification for all repos.
	TestCommand   string     `json:"test_command,omitempty"`
	Status        TaskStatus `json:"status"`
	FailureReason string     `json:"failure_reason,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	State         TaskState  `json:"state"`
}

// ---------- Events ----------

// Event is one append-only record of something that happened to a task.
// Seq is monotonically increasing per task.
type Event struct {
	Seq    int64          `json:"seq"`
	TaskID string         `json:"task_id"`
	Type   string         `json:"type"`
	At     time.Time      `json:"at"`
	Data   map[string]any `json:"data,omitempty"`
}

// Event types. Data payloads are documented next to the emitting code.
const (
	EvTaskCreated       = "task.created"
	EvWorkspacePrepared = "workspace.prepared"
	EvPhaseChanged      = "task.phase_changed"
	EvStepPlanned       = "step.planned"
	EvRouteChosen       = "route.chosen"
	EvAgentStarted      = "agent.started"
	EvAgentCompleted    = "agent.completed"
	EvAgentFailed       = "agent.failed"
	EvFilesChanged      = "files.changed"
	EvTestsStarted      = "tests.started"
	EvTestsPassed       = "tests.passed"
	EvTestsFailed       = "tests.failed"
	EvTestsSkipped      = "tests.skipped"
	EvReviewStarted     = "review.started"
	EvReviewFinding     = "review.finding"
	EvReviewCompleted   = "review.completed"
	EvDecisionRequired  = "decision.required"
	EvDecisionResolved  = "decision.resolved"
	EvEvidenceAdded     = "evidence.added"
	EvTaskCompleted     = "task.completed"
	EvTaskFailed        = "task.failed"
	EvTaskInterrupted   = "task.interrupted"
	EvTaskResumed       = "task.resumed"
	EvWarning           = "warning"
)

// ---------- Decisions ----------

type DecisionOption struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
	// Effect tells the engine what choosing this option does:
	//   ""       — continue the workflow at the decision's ReturnTo phase
	//   "abort"  — fail the task
	//   "accept" — complete the task as-is (human overrides the workflow)
	//   "extend" — reset the step budget and continue at ReturnTo
	Effect string `json:"effect,omitempty"`
}

// DecisionRequest is what an agent emits when it needs a human choice.
type DecisionRequest struct {
	Importance     string           `json:"importance"` // low | medium | high
	Question       string           `json:"question"`
	Recommendation string           `json:"recommendation,omitempty"`
	Reason         string           `json:"reason,omitempty"`
	Options        []DecisionOption `json:"options,omitempty"`
}

type Decision struct {
	ID             string           `json:"id"`
	TaskID         string           `json:"task_id"`
	Importance     string           `json:"importance"`
	Question       string           `json:"question"`
	Recommendation string           `json:"recommendation,omitempty"`
	Reason         string           `json:"reason,omitempty"`
	Options        []DecisionOption `json:"options,omitempty"`
	Status         string           `json:"status"` // open | resolved
	ChosenOption   string           `json:"chosen_option,omitempty"`
	Note           string           `json:"note,omitempty"`
	// ReturnTo is the phase the workflow continues from after resolution.
	ReturnTo   TaskStatus `json:"return_to"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// ---------- Evidence ----------

// EvidenceLevel expresses how strongly a claim about the task outcome is
// supported. Ordered weakest → strongest.
type EvidenceLevel string

const (
	EvidenceAssumed       EvidenceLevel = "assumed"        // someone believes it
	EvidenceCodeInspected EvidenceLevel = "code_inspected" // an agent read the relevant code
	EvidenceReproduced    EvidenceLevel = "reproduced"     // failure reproduced before the change
	EvidenceImplemented   EvidenceLevel = "implemented"    // a concrete diff exists
	EvidenceTested        EvidenceLevel = "tested"         // automated tests pass after the change
	EvidenceReviewed      EvidenceLevel = "reviewed"       // independent reviewer approved
)

// Rank orders evidence levels; higher is stronger.
func (l EvidenceLevel) Rank() int {
	switch l {
	case EvidenceAssumed:
		return 1
	case EvidenceCodeInspected:
		return 2
	case EvidenceReproduced:
		return 3
	case EvidenceImplemented:
		return 4
	case EvidenceTested:
		return 5
	case EvidenceReviewed:
		return 6
	}
	return 0
}

type Evidence struct {
	ID     string        `json:"id"`
	TaskID string        `json:"task_id"`
	Claim  string        `json:"claim"`
	Level  EvidenceLevel `json:"level"`
	Source string        `json:"source"` // agent run id / "tester" / "human"
	Detail string        `json:"detail,omitempty"`
	At     time.Time     `json:"at"`
}

// ---------- Agent runs (observability) ----------

type AgentRun struct {
	ID           string     `json:"id"`
	TaskID       string     `json:"task_id"`
	Role         string     `json:"role"`
	Phase        TaskStatus `json:"phase"`
	Executor     string     `json:"executor"`
	Model        string     `json:"model,omitempty"`
	RouteReason  string     `json:"route_reason,omitempty"` // why this executor/model was chosen
	Attempt      int        `json:"attempt"`
	Status       string     `json:"status"` // running | ok | error
	Summary      string     `json:"summary,omitempty"`
	Error        string     `json:"error,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	DurationMS   int64      `json:"duration_ms"`
	InputTokens  int        `json:"input_tokens,omitempty"`
	OutputTokens int        `json:"output_tokens,omitempty"`
	NumTurns     int        `json:"num_turns,omitempty"`
	CostUSD      float64    `json:"cost_usd,omitempty"`
}

// ---------- Role outputs ----------

// ResearchOutput is the structured result of a Researcher run.
type ResearchOutput struct {
	Summary         string           `json:"summary"`
	KeyFiles        []string         `json:"key_files,omitempty"`
	Uncertainty     string           `json:"uncertainty"` // low | medium | high
	Risks           []string         `json:"risks,omitempty"`
	OpenQuestions   []string         `json:"open_questions,omitempty"`
	DecisionRequest *DecisionRequest `json:"decision_request,omitempty"`
}

// DevelopOutput is the structured result of a Developer run.
type DevelopOutput struct {
	Status          string           `json:"status"` // completed | blocked | uncertain
	Summary         string           `json:"summary"`
	FilesChanged    []string         `json:"files_changed,omitempty"`
	Notes           string           `json:"notes,omitempty"`
	DecisionRequest *DecisionRequest `json:"decision_request,omitempty"`
}

// ReviewOutput is the structured result of a Reviewer run.
type ReviewOutput struct {
	Verdict  string    `json:"verdict"` // approve | changes_requested
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings,omitempty"`
}

// ---------- IDs ----------

// NewID returns a short random identifier with a type prefix, e.g. "task_a1b2c3d4".
func NewID(prefix string) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	return prefix + "_" + hex.EncodeToString(b)
}
