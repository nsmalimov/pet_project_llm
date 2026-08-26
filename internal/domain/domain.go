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

// TaskKind tells the engine what the baseline is expected to show.
type TaskKind string

const (
	KindBugfix TaskKind = "bugfix" // baseline must fail to count as reproduced
	KindChange TaskKind = "change" // baseline is informational
)

// TestResult is the outcome of one verification run.
type TestResult struct {
	Repo       string `json:"repo"`
	Command    string `json:"command"`
	Passed     bool   `json:"passed"`
	ExitCode   int    `json:"exit_code"`
	OutputTail string `json:"output_tail"`
	// Output is the full captured output (capped by the runner); persisted in
	// artifacts, not in the task snapshot.
	Output string `json:"-"`
}

// RootCause is a researcher hypothesis pointing at a concrete location.
type RootCause struct {
	Statement string `json:"statement"`
	File      string `json:"file,omitempty"` // repoName/path
	Line      int    `json:"line,omitempty"`
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

	// Baseline holds the reproduction run(s) executed on the untouched
	// worktree before any implementation. BaselineDone guards idempotency on
	// resume. The failing output is fed to the developer.
	Baseline     []TestResult `json:"baseline,omitempty"`
	BaselineDone bool         `json:"baseline_done,omitempty"`
	// RootCause is the researcher's hypothesis; it is NOT evidence by itself —
	// the proof packet cross-checks it against the diff and the test runs.
	RootCause *RootCause `json:"root_cause,omitempty"`
	// Commits maps repo name -> SHA of the last engine-made commit of the
	// developer's change in that worktree.
	Commits map[string]string `json:"commits,omitempty"`

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
	TestCommand string `json:"test_command,omitempty"`
	// ReproCommand, if set, is the narrow command expected to FAIL before the
	// change and PASS after it (e.g. `go test -run TestX ./...`). It runs on
	// the untouched baseline and again after implementation. When empty the
	// test command doubles as the reproduction command.
	ReproCommand string `json:"repro_command,omitempty"`
	// Kind is "bugfix" (baseline is expected to fail) or "change".
	Kind          TaskKind   `json:"kind,omitempty"`
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
	EvBaselineStarted   = "baseline.started"
	EvBaselineFailed    = "baseline.failed" // expected for bugfix: the problem reproduced
	EvBaselinePassed    = "baseline.passed"
	EvBaselineSkipped   = "baseline.skipped"
	EvArtifactAdded     = "artifact.added"
	EvCommitted         = "workspace.committed"
	EvPacketBuilt       = "packet.built"
	EvVerdictRecorded   = "verdict.recorded"
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

// ---------- Evidence artifacts ----------

// ArtifactKind classifies raw proof material. Every artifact is written by
// the step that produced the fact, never synthesised afterwards.
type ArtifactKind string

const (
	ArtBaselineRun ArtifactKind = "baseline_run" // test/repro command on the untouched worktree
	ArtTestRun     ArtifactKind = "test_run"     // test/repro command after the change
	ArtDiff        ArtifactKind = "diff"         // the change itself: files, diff, commits
	ArtReview      ArtifactKind = "review"       // independent reviewer output
	ArtRootCause   ArtifactKind = "root_cause"   // researcher hypothesis (agent-reported)
)

// Artifact is an immutable, append-only piece of raw evidence.
type Artifact struct {
	ID     string       `json:"id"`
	TaskID string       `json:"task_id"`
	Kind   ArtifactKind `json:"kind"`
	Title  string       `json:"title"`
	Phase  TaskStatus   `json:"phase,omitempty"`
	RunID  string       `json:"run_id,omitempty"` // producing agent run, if any
	At     time.Time    `json:"at"`

	// Command runs (baseline_run, test_run).
	Repo     string `json:"repo,omitempty"`
	Command  string `json:"command,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Passed   *bool  `json:"passed,omitempty"`
	Output   string `json:"output,omitempty"`
	// Narrow reports whether the command is the task's repro command (as
	// opposed to the full test suite).
	Narrow bool `json:"narrow,omitempty"`

	// Change (diff).
	Files   []string          `json:"files,omitempty"`
	Diff    string            `json:"diff,omitempty"`
	Commits map[string]string `json:"commits,omitempty"` // repo -> sha
	Branch  string            `json:"branch,omitempty"`
	Model   string            `json:"model,omitempty"` // author / reviewer model

	// Review.
	Verdict        string    `json:"verdict,omitempty"`
	Summary        string    `json:"summary,omitempty"`
	Findings       []Finding `json:"findings,omitempty"`
	Checked        []string  `json:"checked,omitempty"`
	NotChecked     []string  `json:"not_checked,omitempty"`
	Counterexample string    `json:"counterexample,omitempty"`

	// Root cause hypothesis.
	RootCause *RootCause `json:"root_cause,omitempty"`
}

// ---------- Claims & proof packet ----------

type ClaimStatus string

const (
	ClaimSupported    ClaimStatus = "supported"
	ClaimInsufficient ClaimStatus = "insufficient"
	ClaimContradicted ClaimStatus = "contradicted"
	ClaimBlocked      ClaimStatus = "blocked"
)

type ClaimType string

const (
	ClaimProblemReproduced    ClaimType = "problem_reproduced"
	ClaimRootCauseSupported   ClaimType = "root_cause_supported"
	ClaimChangeVerified       ClaimType = "change_verified"
	ClaimIndependentChallenge ClaimType = "independent_challenge"
	ClaimIntegrationChecked   ClaimType = "integration_checked"
	ClaimCrossServiceImpact   ClaimType = "cross_service_impact"
)

// Claim is one statement about the change with a status derived strictly from
// persisted artifacts. Reason explains the status in one sentence; Gap says
// what evidence is missing when the claim is not supported.
type Claim struct {
	Type        ClaimType   `json:"type"`
	Title       string      `json:"title"`
	Status      ClaimStatus `json:"status"`
	Core        bool        `json:"core"` // participates in the overall verdict
	Statement   string      `json:"statement"`
	Reason      string      `json:"reason"`
	Gap         string      `json:"gap,omitempty"`
	ArtifactIDs []string    `json:"artifact_ids,omitempty"`
	EvidenceIDs []string    `json:"evidence_ids,omitempty"`
}

// Risk is an unresolved concern. Source tells the reader whether it comes
// from an artifact (reviewer finding, missing check) or is agent-reported.
type Risk struct {
	Severity string `json:"severity"` // high | medium | low | unknown
	Source   string `json:"source"`   // reviewer | engine | researcher (agent-reported)
	Text     string `json:"text"`
	File     string `json:"file,omitempty"`
}

// ChangeSummary is the "what changed" block of a packet.
type ChangeSummary struct {
	Files        []string          `json:"files"`
	TestFiles    []string          `json:"test_files,omitempty"`
	Branch       string            `json:"branch,omitempty"`
	Commits      map[string]string `json:"commits,omitempty"`
	AuthorModel  string            `json:"author_model,omitempty"`
	DiffArtifact string            `json:"diff_artifact,omitempty"`
}

// Packet is the versioned, immutable change-assurance record. It is derived
// from persisted artifacts only; a rebuild that yields the same content
// keeps the same version.
type Packet struct {
	TaskID      string        `json:"task_id"`
	Version     int           `json:"version"`
	Fingerprint string        `json:"fingerprint"`
	BuiltAt     time.Time     `json:"built_at"`
	TaskStatus  TaskStatus    `json:"task_status"`
	Verdict     ClaimStatus   `json:"verdict"` // supported | insufficient | blocked
	VerdictWhy  string        `json:"verdict_why"`
	Change      ChangeSummary `json:"change"`
	Claims      []Claim       `json:"claims"`
	Gaps        []string      `json:"gaps"`
	Risks       []Risk        `json:"risks"`
	Confidence  string        `json:"confidence"` // strongest evidence level (legacy chain)
}

// Verdict is the human merge decision on a packet version. It is recorded
// separately from workflow decisions and from any agent verdict.
type Verdict struct {
	ID            string    `json:"id"`
	TaskID        string    `json:"task_id"`
	PacketVersion int       `json:"packet_version"`
	Decision      string    `json:"decision"` // accept | request_changes | reject
	Note          string    `json:"note,omitempty"`
	By            string    `json:"by,omitempty"`
	At            time.Time `json:"at"`
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
	RootCause       *RootCause       `json:"root_cause,omitempty"`
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
	// Checked / NotChecked make the reviewer's coverage explicit: what it
	// actually verified and what it did not. NotChecked items become gaps.
	Checked    []string `json:"checked,omitempty"`
	NotChecked []string `json:"not_checked,omitempty"`
	// Counterexample is a concrete scenario the reviewer believes still
	// breaks, if any.
	Counterexample string `json:"counterexample,omitempty"`
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
