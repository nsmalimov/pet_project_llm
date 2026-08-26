package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"orchestrator/internal/domain"
	"orchestrator/internal/gitws"
	"orchestrator/internal/integration"
	"orchestrator/internal/proof"
	"orchestrator/internal/repos"
	"orchestrator/internal/sandbox"
	"orchestrator/internal/verify"
)

// This file holds the Proofline additions to the engine: baseline
// reproduction, artifact capture, packet snapshots and human verdicts. The
// workflow itself (steps.go) is unchanged apart from calling into these.

// TaskSpec is the full creation request.
type TaskSpec struct {
	Goal         string
	Context      []string
	Repos        []string
	TestCommand  string
	ReproCommand string
	Kind         domain.TaskKind // empty → inferred from the goal
	HeadRef      string          // verify-only mode: existing change to verify
	PR           *domain.PullRequestRef
	// IdempotencyKey makes creation replay-safe: the same key returns the
	// task created the first time (Existing=true) without a second run.
	IdempotencyKey string
	// WorkspaceID is the authorization scope; when a repository registry is
	// used, every repo must belong to this workspace (or be unscoped).
	WorkspaceID string
	// PinnedBase is the commit worktrees are created from (PR base).
	PinnedBase string
	// Scenario marks a Local Pilot example (scripted agents).
	Scenario string
	forcedID string
}

// safeRef: a SHA or a plain branch/tag name — never something git could read
// as an option.
var safeRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

// CreateTaskIdempotent is CreateTaskSpec with duplicate suppression. The
// second return value reports whether an existing task was returned.
func (e *Engine) CreateTaskIdempotent(spec TaskSpec) (*domain.Task, bool, error) {
	if spec.IdempotencyKey == "" {
		t, err := e.CreateTaskSpec(spec)
		return t, false, err
	}
	// Reserve the key with a provisional ID first so two concurrent creators
	// cannot both create; the loser returns the winner's task.
	id := domain.NewID("task")
	owner, dup, err := e.Store.ClaimIdempotencyKey(spec.WorkspaceID+"|"+spec.IdempotencyKey, id)
	if err != nil {
		return nil, false, err
	}
	if dup {
		// The winner reserved the key before creating the task; give it a
		// moment to finish. A reservation without a task after that is a
		// crashed creator: surface it, never create a second task.
		deadline := time.Now().Add(10 * time.Second)
		for {
			t, err := e.Store.GetTask(owner)
			if err == nil {
				return t, true, nil
			}
			if time.Now().After(deadline) {
				return nil, true, fmt.Errorf("idempotency key %q is reserved by task %s which was never created (creator crashed?)", spec.IdempotencyKey, owner)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	spec.forcedID = id
	t, err := e.CreateTaskSpec(spec)
	return t, false, err
}

var bugfixWords = regexp.MustCompile(`(?i)\b(fix|bug|regress|broken|crash|incorrect|wrong|duplicate|fails?|leak|race)\b`)

// InferKind guesses bugfix vs change from the goal text. Explicit --kind wins.
func InferKind(goal string) domain.TaskKind {
	if bugfixWords.MatchString(goal) {
		return domain.KindBugfix
	}
	return domain.KindChange
}

// CreateTaskSpec is CreateTask with the Proofline fields.
func (e *Engine) CreateTaskSpec(spec TaskSpec) (*domain.Task, error) {
	switch spec.Kind {
	case "", domain.KindBugfix, domain.KindChange:
	default:
		return nil, fmt.Errorf("unknown task kind %q (bugfix|change)", spec.Kind)
	}
	if e.Repos != nil {
		for _, ref := range spec.Repos {
			if !strings.HasPrefix(ref, "repo_") {
				continue
			}
			rp, err := e.Repos.Get(ref)
			if err != nil {
				return nil, err
			}
			if spec.WorkspaceID != "" && rp.Workspace != "" && rp.Workspace != spec.WorkspaceID {
				return nil, fmt.Errorf("repository %s belongs to another workspace", ref)
			}
			// Repository policy decides how the repository is verified. A
			// request may only omit the commands (policy fills them) or, in
			// LOCAL_UNSAFE, override them visibly.
			if rp.Policy != nil {
				if spec.TestCommand == "" {
					spec.TestCommand = rp.Policy.TestCommand
				}
				if spec.ReproCommand == "" {
					spec.ReproCommand = rp.Policy.ReproCommand
				}
				if e.Policy.Mode == sandbox.ModeSafe && ((spec.TestCommand != "" && spec.TestCommand != rp.Policy.TestCommand) || (spec.ReproCommand != "" && spec.ReproCommand != rp.Policy.ReproCommand)) {
					return nil, fmt.Errorf("SAFE_SANDBOX: commands must match the repository policy of %s", ref)
				}
			} else if e.Policy.Mode == sandbox.ModeSafe && (spec.TestCommand != "" || spec.ReproCommand != "") {
				return nil, fmt.Errorf("SAFE_SANDBOX: repository %s has no policy; ad-hoc commands are refused", ref)
			}
			if rp.Policy != nil {
				for _, c := range []string{spec.TestCommand, spec.ReproCommand} {
					if c != "" && !rp.Policy.RunnerAllowed(c) {
						return nil, fmt.Errorf("repository policy of %s does not allow runner of %q", ref, c)
					}
				}
				if !rp.Policy.AgentMayWrite() && spec.HeadRef == "" {
					return nil, fmt.Errorf("repository %s is verify-only by policy (agent_write=false): provide an existing head ref", ref)
				}
			}
		}
	}
	t, err := e.createTask(spec.Goal, spec.Context, spec.Repos, spec.TestCommand, spec.forcedID)
	if err != nil {
		return nil, err
	}
	t.WorkspaceID = spec.WorkspaceID
	t.Scenario = spec.Scenario
	t.ReproCommand = strings.TrimSpace(spec.ReproCommand)
	if t.ReproCommand != "" {
		if _, err := e.Policy.ValidateCommand(t.ReproCommand); err != nil {
			return nil, fmt.Errorf("repro command: %w", err)
		}
	}
	t.HeadRef = strings.TrimSpace(spec.HeadRef)
	if t.HeadRef != "" && !safeRef.MatchString(t.HeadRef) {
		return nil, fmt.Errorf("head ref %q is not a safe git ref", t.HeadRef)
	}
	if spec.PinnedBase != "" {
		if !safeRef.MatchString(spec.PinnedBase) {
			return nil, fmt.Errorf("base %q is not a safe git ref", spec.PinnedBase)
		}
		t.State.PinnedBase = spec.PinnedBase
	}
	t.PR = spec.PR
	t.Kind = spec.Kind
	if t.Kind == "" {
		t.Kind = InferKind(spec.Goal)
	}
	if err := e.Store.SaveTask(t); err != nil {
		return nil, err
	}
	return t, nil
}

// ---------- artifacts ----------

func (e *Engine) addArtifact(t *domain.Task, a domain.Artifact) domain.Artifact {
	a.ID = domain.NewID("art")
	a.TaskID = t.ID
	a.Phase = t.Status
	a.At = time.Now().UTC()
	a.WorktreeRoot = t.State.WorktreeRoot
	a.ExecMode = string(e.Policy.Mode)
	// Persist nothing unbounded or secret-bearing.
	cap := e.Policy.MaxArtifact
	if cap == 0 {
		cap = 512 * 1024
	}
	for _, f := range []*string{&a.Output, &a.Diff, &a.Summary, &a.Counterexample} {
		if len(*f) > cap {
			*f = (*f)[:cap] + "\n…[artifact truncated by policy]…"
			a.Truncated = true
		}
		red, n := sandbox.Redact(*f)
		*f, a.Redacted = red, a.Redacted+n
	}
	if a.Producer == "" {
		a.Producer = "engine." + string(t.Status)
	}
	if a.SourceSHAs == nil {
		// Bind the artifact to the exact source state it was observed on.
		heads, dirty, err := e.WS.Heads(context.Background(), t)
		if err == nil {
			a.SourceSHAs, a.SourceDirty = heads, dirty
		}
	}
	if err := e.Store.AddArtifact(a); err != nil {
		e.emit(t.ID, domain.EvWarning, map[string]any{"warning": "artifact not persisted", "error": err.Error(), "kind": string(a.Kind)})
		return a
	}
	e.emit(t.ID, domain.EvArtifactAdded, map[string]any{"artifact_id": a.ID, "kind": string(a.Kind), "title": a.Title})
	return a
}

func boolp(b bool) *bool { return &b }

// runArtifact converts a test result into a run artifact.
func runArtifact(kind domain.ArtifactKind, title string, res domain.TestResult, narrow bool, repeat int) domain.Artifact {
	return domain.Artifact{
		Kind: kind, Title: title, Repo: res.Repo, Command: res.Command, Effective: res.Effective,
		ExitCode: res.ExitCode, Passed: boolp(res.Passed), Output: res.Output, Narrow: narrow,
		TimedOut: res.TimedOut, Tests: res.Tests, TestsParsed: res.TestsParsed, Repeat: repeat,
		Truncated: res.Truncated, Redacted: res.Redacted,
	}
}

// verificationCommands returns the (command, narrow) pairs to run in a repo:
// the repro command first when it differs from the test command.
type verifyCmd struct {
	Cmd    string
	Narrow bool
}

func verificationCommands(t *domain.Task, dir string) []verifyCmd {
	type pair = verifyCmd
	var out []pair
	test := t.TestCommand
	if test == "" {
		test = verify.DetectCommand(dir)
	}
	if t.ReproCommand != "" && t.ReproCommand != test {
		out = append(out, pair{t.ReproCommand, true})
	}
	if test != "" {
		out = append(out, pair{test, false})
	}
	return out
}

// repoPolicy returns the registered policy for a task repo, if any.
func (e *Engine) repoPolicy(r domain.RepoRef) *repos.Policy {
	if e.Repos == nil {
		return nil
	}
	list, err := e.Repos.List()
	if err != nil {
		return nil
	}
	for _, rp := range list {
		if rp.Path == r.Path {
			return rp.Policy
		}
	}
	return nil
}

// integrationConfigured reports whether any task repo has an integration check.
func (e *Engine) integrationConfigured(t *domain.Task) bool {
	for _, r := range t.Repos {
		if p := e.repoPolicy(r); p != nil && p.Integration != nil {
			return true
		}
	}
	return false
}

// runIntegration starts the service from each worktree with an integration
// policy and probes it. Artifacts are bound to the source state captured
// before the run. Returns the results (nil when nothing is configured).
func (e *Engine) runIntegration(ctx context.Context, t *domain.Task, title string) ([]domain.Artifact, error) {
	var out []domain.Artifact
	for _, r := range t.Repos {
		pol := e.repoPolicy(r)
		if pol == nil || pol.Integration == nil {
			continue
		}
		heads, dirty, err := e.WS.Heads(ctx, t)
		if err != nil {
			return out, err
		}
		dir := gitws.RepoDir(t, r)
		e.emit(t.ID, domain.EvTestsStarted, map[string]any{"repo": r.Name, "integration": pol.Integration.Start, "checks": len(pol.Integration.Checks)})
		res := integration.Run(ctx, e.Policy, dir, pol.Integration)
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		passed := res.Passed && res.Err == ""
		a := domain.Artifact{
			Kind: domain.ArtIntegrationRun, Title: title + ": " + pol.Integration.Start, Repo: r.Name,
			Command: pol.Integration.Start, Passed: boolp(passed), Output: res.Output, Redacted: res.Redacted,
			Tests: res.Checks, TestsParsed: true, Producer: "engine.integration",
		}
		if res.Err != "" {
			a.ExitCode = -1
		}
		a.TimedOut = res.Outcome == "timeout"
		a.Summary = res.Outcome // pass | fail | unavailable | timeout
		e.bindSource(ctx, t, &a, heads, dirty)
		a = e.addArtifact(t, a)
		out = append(out, a)
		if passed {
			e.emit(t.ID, domain.EvTestsPassed, map[string]any{"repo": r.Name, "integration": pol.Integration.Start, "checks": len(res.Checks)})
		} else {
			e.emit(t.ID, domain.EvTestsFailed, map[string]any{"repo": r.Name, "integration": pol.Integration.Start, "output_tail": tailStr(res.Output, 1500), "error": res.Err})
		}
	}
	return out, nil
}

// runBaseline executes the repro/test commands on the untouched worktrees
// exactly once per task and records the outcome as artifacts. For bugfix
// tasks a failing baseline is the reproduction; a passing one is recorded as
// a contradiction (the test does not exercise the bug) — the packet decides.
func (e *Engine) runBaseline(ctx context.Context, t *domain.Task) error {
	if t.State.BaselineDone {
		return nil
	}
	heads, dirty, err := e.WS.Heads(ctx, t)
	if err != nil {
		return err
	}
	var results []domain.TestResult
	ran := 0
	for _, r := range t.Repos {
		dir := gitws.RepoDir(t, r)
		for _, c := range verificationCommands(t, dir) {
			ran++
			e.emit(t.ID, domain.EvBaselineStarted, map[string]any{"repo": r.Name, "command": c.Cmd, "narrow": c.Narrow})
			res := verify.Run(ctx, e.Policy, r.Name, dir, c.Cmd, e.Cfg.TestTimeout)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			results = append(results, res)
			a := runArtifact(domain.ArtBaselineRun, "baseline: "+c.Cmd, res, c.Narrow, 0)
			e.bindSource(ctx, t, &a, heads, dirty)
			e.addArtifact(t, a)
			if res.Passed {
				e.emit(t.ID, domain.EvBaselinePassed, map[string]any{"repo": r.Name, "command": c.Cmd})
			} else {
				e.emit(t.ID, domain.EvBaselineFailed, map[string]any{
					"repo": r.Name, "command": c.Cmd, "exit_code": res.ExitCode, "output_tail": tailStr(res.OutputTail, 1500),
				})
			}
		}
	}
	if _, err := e.runIntegration(ctx, t, "integration baseline"); err != nil {
		return err
	}
	t.State.Baseline = results
	t.State.BaselineDone = true
	if ran == 0 {
		e.emit(t.ID, domain.EvBaselineSkipped, map[string]any{"reason": "no test or repro command detected in any repo"})
	} else if t.Kind == domain.KindBugfix && len(failedOnly(results)) > 0 {
		e.addEvidence(t, domain.EvidenceReproduced,
			"the reported failure reproduces on the unchanged code", "tester", summarizeTests(results))
	}
	return e.Store.SaveTask(t)
}

// ---------- packets & verdicts ----------

// BuildPacket derives the packet from persisted state and appends it as a
// new version when its content changed. Safe to call at any time.
func (e *Engine) BuildPacket(taskID string) (*domain.Packet, error) {
	t, err := e.Store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	return e.snapshotPacket(t)
}

func (e *Engine) snapshotPacket(t *domain.Task) (*domain.Packet, error) {
	arts, err := e.Store.Artifacts(t.ID)
	if err != nil {
		return nil, err
	}
	evidence, _ := e.Store.EvidenceList(t.ID)
	runs, _ := e.Store.Runs(t.ID)
	decisions, _ := e.Store.Decisions(t.ID)
	in := proof.Input{
		Task: t, Artifacts: arts, Evidence: evidence, Runs: runs, Decisions: decisions,
		FileExists: proof.WorktreeFileExists(t), IntegrationConfigured: e.integrationConfigured(t),
	}
	if t.State.WorktreeRoot != "" {
		if heads, dirty, err := e.WS.Heads(context.Background(), t); err == nil {
			in.CurrentSHAs, in.CurrentDirty = heads, dirty
		} else {
			in.SourceUnknown = true
		}
	}
	fresh := proof.Build(in)
	fresh.ExecMode = string(e.Policy.Mode)
	if t.Status.Active() {
		// While the engine is running, the tree changes constantly; a
		// persisted version per poll would be noise and would race the
		// worker's own snapshot. Return the latest persisted version, or a
		// live preview (version 0) if none exists.
		if existing, err := e.Store.Packets(t.ID); err == nil && len(existing) > 0 {
			l := existing[len(existing)-1]
			l.Live = true
			return &l, nil
		}
		fresh.Live = true
		return &fresh, nil
	}
	var p domain.Packet
	err = e.Store.WithPacketLock(t.ID, func() error {
		existing, err := e.Store.Packets(t.ID)
		if err != nil {
			return err
		}
		var isNew bool
		p, isNew = proof.NextVersion(existing, fresh)
		if !isNew {
			return nil
		}
		if err := e.Store.AddPacket(p); err != nil {
			return err
		}
		e.emit(t.ID, domain.EvPacketBuilt, map[string]any{
			"version": p.Version, "verdict": string(p.Verdict), "gaps": len(p.Gaps), "fingerprint": p.Fingerprint,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// PacketView is what the Change Case screen renders.
type PacketView struct {
	Task      *domain.Task      `json:"task"`
	Packet    *domain.Packet    `json:"packet"`
	Versions  []packetVersion   `json:"versions"`
	Artifacts []domain.Artifact `json:"artifacts"`
	Verdicts  []domain.Verdict  `json:"verdicts"`
	Runs      []domain.AgentRun `json:"runs"`
	Evidence  []domain.Evidence `json:"evidence"`
	Decisions []domain.Decision `json:"decisions"`
	Totals    Totals            `json:"totals"`
}

type packetVersion struct {
	Version     int                `json:"version"`
	BuiltAt     time.Time          `json:"built_at"`
	Verdict     domain.ClaimStatus `json:"verdict"`
	TaskStatus  domain.TaskStatus  `json:"task_status"`
	Fingerprint string             `json:"fingerprint"`
}

// PacketState returns the latest packet (building one if the task changed)
// plus everything needed to drill down to raw evidence.
func (e *Engine) PacketState(taskID string) (*PacketView, error) {
	t, err := e.Store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	p, err := e.snapshotPacket(t)
	if err != nil {
		return nil, err
	}
	fs, err := e.FullState(taskID)
	if err != nil {
		return nil, err
	}
	arts, _ := e.Store.Artifacts(taskID)
	verdicts, _ := e.Store.Verdicts(taskID)
	all, _ := e.Store.Packets(taskID)
	v := &PacketView{Task: t, Packet: p, Artifacts: arts, Verdicts: verdicts,
		Runs: fs.Runs, Evidence: fs.Evidence, Decisions: fs.Decisions, Totals: fs.Totals}
	for _, x := range all {
		v.Versions = append(v.Versions, packetVersion{x.Version, x.BuiltAt, x.Verdict, x.TaskStatus, x.Fingerprint})
	}
	if v.Artifacts == nil {
		v.Artifacts = []domain.Artifact{}
	}
	if v.Verdicts == nil {
		v.Verdicts = []domain.Verdict{}
	}
	if v.Versions == nil {
		v.Versions = []packetVersion{}
	}
	if v.Runs == nil {
		v.Runs = []domain.AgentRun{}
	}
	if v.Evidence == nil {
		v.Evidence = []domain.Evidence{}
	}
	if v.Decisions == nil {
		v.Decisions = []domain.Decision{}
	}
	return v, nil
}

// PacketVersion returns one historical packet version.
func (e *Engine) PacketVersion(taskID string, version int) (*domain.Packet, error) {
	all, err := e.Store.Packets(taskID)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Version == version {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("packet version %d not found", version)
}

var (
	ErrTaskRunning   = errors.New("task is still running; a verdict needs a finished packet")
	ErrPacketChanged = errors.New("the packet changed since it was viewed; re-read before deciding")
)

// RecordVerdict stores the human merge decision against the packet version
// the human actually saw. expectedVersion > 0 must equal the current packet
// version — if the evidence or the code changed in between, the decision is
// refused rather than silently attached to a packet nobody reviewed. It is
// also refused while the workflow is executing.
func (e *Engine) RecordVerdict(taskID, decision, note, by string, expectedVersion int) (*domain.Verdict, error) {
	switch decision {
	case "accept", "request_changes", "reject":
	default:
		return nil, fmt.Errorf("unknown decision %q (accept|request_changes|reject)", decision)
	}
	t, err := e.Store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	if t.Status.Active() {
		return nil, ErrTaskRunning
	}
	p, err := e.snapshotPacket(t)
	if err != nil {
		return nil, err
	}
	if expectedVersion > 0 && p.Version != expectedVersion {
		return nil, fmt.Errorf("%w: viewed v%d, current v%d (%s)", ErrPacketChanged, expectedVersion, p.Version, p.Verdict)
	}
	v := domain.Verdict{
		ID: domain.NewID("ver"), TaskID: taskID, PacketVersion: p.Version,
		Decision: decision, Note: strings.TrimSpace(note), By: by, At: time.Now().UTC(),
	}
	if err := e.Store.AddVerdict(v); err != nil {
		return nil, err
	}
	e.emit(taskID, domain.EvVerdictRecorded, map[string]any{
		"verdict_id": v.ID, "decision": decision, "packet_version": p.Version, "note": v.Note, "by": by,
	})
	return &v, nil
}

// applyHead (verify-only mode) moves the worktrees to the head ref after the
// baseline ran on the base, and records the existing change as the diff
// artifact. No developer runs; the author is external.
func (e *Engine) applyHead(ctx context.Context, t *domain.Task) error {
	if t.HeadRef == "" || len(t.State.Commits) > 0 {
		return nil
	}
	shas, err := e.WS.ApplyHead(ctx, t, t.HeadRef)
	if err != nil {
		return err
	}
	t.State.Commits = shas
	diff, files, err := e.WS.Diff(ctx, t)
	if err != nil {
		return fmt.Errorf("compute diff: %w", err)
	}
	t.State.ChangedFiles = files
	t.State.AuthorModel = "external"
	e.emit(t.ID, domain.EvHeadApplied, map[string]any{"ref": t.HeadRef, "commits": shas, "files": files})
	e.emit(t.ID, domain.EvFilesChanged, map[string]any{"files": files, "count": len(files)})
	if len(files) > 0 {
		e.addArtifact(t, domain.Artifact{
			Kind: domain.ArtDiff, Title: fmt.Sprintf("change under review: %d file(s) at %s", len(files), t.HeadRef),
			Files: files, Diff: diff, Commits: shas, Branch: t.State.Branch, Model: "external",
			Producer: "engine.head_applied",
		})
		e.addEvidence(t, domain.EvidenceImplemented, "an existing change is under verification", "external", fmt.Sprintf("%d file(s) at %s", len(files), t.HeadRef))
	}
	return e.Store.SaveTask(t)
}

// bindSource stamps the artifact with the source state captured BEFORE the
// command ran and re-checks it afterwards. If the worktree moved during the
// run the artifact is marked dirty (never current) instead of being
// mislabelled with whichever SHA happened to be there at the end.
func (e *Engine) bindSource(ctx context.Context, t *domain.Task, a *domain.Artifact, before map[string]string, dirtyBefore bool) {
	a.SourceSHAs, a.SourceDirty = before, dirtyBefore
	after, dirtyAfter, err := e.WS.Heads(ctx, t)
	if err != nil {
		a.SourceDirty = true
		return
	}
	if dirtyAfter {
		a.SourceDirty = true
	}
	for k, v := range before {
		if after[k] != v {
			a.SourceDirty = true
			e.emit(t.ID, domain.EvWarning, map[string]any{"warning": "source state changed while a command was running; artifact marked not current", "artifact": a.Title})
		}
	}
}

var ErrNotReverifiable = errors.New("task is running or awaiting a decision; cannot re-verify now")

// Reverify re-runs verification and independent review on the worktree as it
// is now (after a later edit, a STALE packet, or a resumed task). Nothing is
// invented: baseline stays, new test/review artifacts are bound to the
// current HEAD and the packet gets a new version.
func (e *Engine) Reverify(id string) (*domain.Task, error) {
	t, err := e.Store.GetTask(id)
	if err != nil {
		return nil, err
	}
	if t.Status.Active() || t.Status == domain.StatusAwaitingDecision {
		return nil, ErrNotReverifiable
	}
	if t.State.WorktreeRoot == "" {
		return nil, errors.New("task has no worktree to verify")
	}
	// Commit whatever is in the worktree so the evidence binds to a SHA.
	if commits, cerr := e.WS.Commit(context.Background(), t, "orc: re-verify current state"); cerr == nil && len(commits) > 0 {
		t.State.Commits = commits
	}
	if diff, files, derr := e.WS.Diff(context.Background(), t); derr == nil {
		t.State.ChangedFiles = files
		if len(files) > 0 {
			// The change under verification is whatever the tree holds now;
			// bind a fresh diff artifact to the new HEAD.
			e.addArtifact(t, domain.Artifact{
				Kind: domain.ArtDiff, Title: fmt.Sprintf("change (re-verified): %d file(s)", len(files)), Producer: "engine.reverify",
				Files: files, Diff: diff, Commits: t.State.Commits, Branch: t.State.Branch, Model: t.State.AuthorModel,
			})
		}
	}
	t.FailureReason = ""
	t.State.FixAttempts, t.State.ReviewRounds = 0, 0
	t.State.ResumeStatus = ""
	if err := e.setStatus(t, domain.StatusVerifying); err != nil {
		return nil, err
	}
	e.emit(t.ID, domain.EvStepPlanned, map[string]any{"action": "verify", "reason": "re-verification requested on the current worktree state"})
	return t, nil
}
