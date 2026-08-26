// Package engine drives a task through the workflow. The workflow is not a
// fixed pipeline: after every step the engine re-plans based on the task's
// persisted state — uncertainty triggers investigation, failed verification
// loops back to implementation with the failure context, review findings loop
// back too, and unanswerable questions pause the task on a structured
// decision. All state lives in the store, never in an LLM context, so the
// engine can resume after a restart.
package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"orchestrator/internal/domain"
	"orchestrator/internal/executor"
	"orchestrator/internal/gitws"
	"orchestrator/internal/memory"
	"orchestrator/internal/repos"
	"orchestrator/internal/roles"
	"orchestrator/internal/router"
	"orchestrator/internal/sandbox"
	"orchestrator/internal/store"
)

type Config struct {
	MaxSteps          int           // hard budget of engine steps per task
	MaxFixAttempts    int           // implement retries after failing tests
	MaxReviewRounds   int           // review → changes_requested loops
	MaxInvestigations int           // deep investigations per task
	AgentTimeout      time.Duration // per agent run
	TestTimeout       time.Duration // per test command
	// RepeatRepro is how many times the narrow repro command runs after the
	// change; disagreeing runs mark the verification flaky (contradicted).
	RepeatRepro int
}

func DefaultConfig() Config {
	return Config{
		MaxSteps:          20,
		MaxFixAttempts:    2,
		MaxReviewRounds:   2,
		MaxInvestigations: 2,
		AgentTimeout:      15 * time.Minute,
		TestTimeout:       10 * time.Minute,
		RepeatRepro:       2,
	}
}

type Engine struct {
	Store  store.Store
	WS     *gitws.Manager
	Execs  map[string]executor.Executor
	Router router.Router
	Mem    memory.Store
	Cfg    Config
	// Policy is the execution boundary every command runs through.
	Policy sandbox.Policy
	// Repos resolves repository IDs/paths; nil → paths are canonicalised
	// under Policy only (tests).
	Repos *repos.Registry

	// OnEvent, if set, receives every appended event (used for live CLI
	// output; a future UI would subscribe the same way).
	OnEvent func(domain.Event)

	mu      sync.Mutex
	running map[string]bool
	cancels map[string]context.CancelFunc
}

var ErrAlreadyRunning = errors.New("task is already running")

func New(st store.Store, ws *gitws.Manager, execs map[string]executor.Executor, rt router.Router, mem memory.Store, cfg Config) *Engine {
	e := &Engine{Store: st, WS: ws, Execs: execs, Router: rt, Mem: mem, Cfg: cfg, running: map[string]bool{}}
	// Default boundary: LOCAL_UNSAFE rooted at the data dir (worktrees live
	// under <data>/worktrees, so they are inside the workspace root).
	if ws != nil {
		if pol, err := sandbox.Default(filepath.Dir(ws.Root)); err == nil {
			e.Policy = pol
			ws.MaxWorktreeBytes = pol.MaxWorktree
		}
	}
	return e
}

// SetPolicy installs an explicit policy (mode, roots, caps).
func (e *Engine) SetPolicy(p sandbox.Policy) {
	e.Policy = p
	if e.WS != nil {
		e.WS.MaxWorktreeBytes = p.MaxWorktree
	}
}

func (e *Engine) emit(taskID, typ string, data map[string]any) {
	ev, err := e.Store.AppendEvent(taskID, typ, data)
	if err != nil {
		return // event loss is logged nowhere better in the prototype
	}
	if e.OnEvent != nil {
		e.OnEvent(ev)
	}
}

// ---------- task lifecycle ----------

// CreateTask validates repos, persists a new pending task and emits
// task.created. It does not start execution.
func (e *Engine) CreateTask(goal string, contextSrcs []string, repoPaths []string, testCommand string) (*domain.Task, error) {
	return e.createTask(goal, contextSrcs, repoPaths, testCommand, "")
}

func (e *Engine) createTask(goal string, contextSrcs []string, repoPaths []string, testCommand string, forcedID string) (*domain.Task, error) {
	if strings.TrimSpace(goal) == "" {
		return nil, errors.New("task goal is required")
	}
	if len(repoPaths) == 0 {
		return nil, errors.New("at least one --repo is required")
	}
	for _, c := range []string{testCommand} {
		if strings.TrimSpace(c) == "" {
			continue
		}
		if _, err := e.Policy.ValidateCommand(c); err != nil {
			return nil, fmt.Errorf("test command: %w", err)
		}
	}
	var repos []domain.RepoRef
	seen := map[string]int{}
	seenPath := map[string]bool{}
	for _, p := range repoPaths {
		abs, err := e.resolveRepo(p)
		if err != nil {
			return nil, err
		}
		if seenPath[abs] {
			return nil, fmt.Errorf("repo %s given more than once", abs)
		}
		seenPath[abs] = true
		name := filepath.Base(abs)
		seen[name]++
		if seen[name] > 1 {
			name = fmt.Sprintf("%s-%d", name, seen[name])
		}
		repos = append(repos, domain.RepoRef{Name: name, Path: abs})
	}
	now := time.Now().UTC()
	id := forcedID
	if id == "" {
		id = domain.NewID("task")
	}
	t := &domain.Task{
		ID:          id,
		Goal:        goal,
		Context:     contextSrcs,
		Repos:       repos,
		TestCommand: testCommand,
		Status:      domain.StatusPending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := e.Store.CreateTask(t); err != nil {
		return nil, err
	}
	e.emit(t.ID, domain.EvTaskCreated, map[string]any{
		"goal": goal, "repos": repoPaths, "context": contextSrcs,
	})
	return t, nil
}

// resolveRepo maps a repo ID or path to a canonical, policy-validated path.
func (e *Engine) resolveRepo(ref string) (string, error) {
	if e.Repos != nil {
		return e.Repos.Resolve(ref)
	}
	if strings.HasPrefix(ref, "repo_") {
		return "", fmt.Errorf("repository IDs need a registry; got %s", ref)
	}
	c, err := sandbox.Canonical(ref)
	if err != nil {
		return "", fmt.Errorf("repository path: %w", err)
	}
	if sandbox.Under(e.Policy.WorkspaceRoot, c) {
		return "", fmt.Errorf("repository %s is inside the Proofline workspace; refusing", c)
	}
	return c, nil
}

// RunTask drives the task until it reaches a terminal state or pauses on a
// decision. Safe to call again after a pause or restart.
func (e *Engine) RunTask(ctx context.Context, id string) error {
	e.mu.Lock()
	if e.running[id] {
		e.mu.Unlock()
		return ErrAlreadyRunning
	}
	e.running[id] = true
	ctx, cancel := context.WithCancel(ctx)
	if e.cancels == nil {
		e.cancels = map[string]context.CancelFunc{}
	}
	e.cancels[id] = cancel
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		delete(e.running, id)
		delete(e.cancels, id)
		e.mu.Unlock()
	}()

	// Cross-process guard: a CLI and a server may share the data dir.
	unlock, err := e.Store.LockTask(id)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAlreadyRunning, err)
	}
	defer unlock()

	for {
		t, err := e.Store.GetTask(id)
		if err != nil {
			return err
		}
		if t.Status.Terminal() || t.Status == domain.StatusAwaitingDecision {
			return nil
		}
		if t.Status == domain.StatusInterrupted {
			return errors.New("task is interrupted; resume it first")
		}
		if t.State.Steps >= e.Cfg.MaxSteps {
			// Pause on a decision instead of discarding the work done so far.
			return e.requireDecision(t, &domain.DecisionRequest{
				Importance: "high",
				Question:   fmt.Sprintf("Step budget exceeded (%d steps). How should the task proceed?", e.Cfg.MaxSteps),
				Reason:     "the workflow looped more than expected",
				Options: []domain.DecisionOption{
					{ID: "extend", Label: "Extend the budget and continue", Effect: "extend"},
					{ID: "abort", Label: "Abort the task", Effect: "abort"},
				},
			}, t.Status, "engine")
		}
		t.State.Steps++

		var stepErr error
		switch t.Status {
		case domain.StatusPending, domain.StatusUnderstanding:
			stepErr = e.stepUnderstand(ctx, t)
		case domain.StatusInvestigating:
			stepErr = e.stepInvestigate(ctx, t)
		case domain.StatusImplementing:
			stepErr = e.stepImplement(ctx, t)
		case domain.StatusVerifying:
			stepErr = e.stepVerify(ctx, t)
		case domain.StatusReviewing:
			stepErr = e.stepReview(ctx, t)
		default:
			stepErr = fmt.Errorf("engine cannot handle status %q", t.Status)
		}
		if stepErr != nil {
			if ctx.Err() != nil {
				// Cancelled/deadline: leave status as-is; restart recovery
				// will mark the task interrupted.
				return stepErr
			}
			if errors.Is(stepErr, store.ErrConflict) {
				// Someone else (restart recovery, revocation, a resolve) moved
				// the task under us. Their write wins; stop this loop.
				e.emit(t.ID, domain.EvWarning, map[string]any{"warning": "engine step lost a concurrent update; stopping", "error": stepErr.Error()})
				return stepErr
			}
			if errors.Is(stepErr, gitws.ErrHostileRepo) {
				// Policy violations are not retried: the repository content
				// itself is the problem. Block visibly.
				e.emit(t.ID, domain.EvPolicyViolation, map[string]any{"error": stepErr.Error()})
				return e.failTask(t, "BLOCKED by execution policy: "+stepErr.Error())
			}
			// A step failure (agent timeout, unparseable output, transient
			// executor error) must not discard the task's accumulated work.
			// Pause on a decision; only fail if even that is impossible.
			if derr := e.requireDecision(t, &domain.DecisionRequest{
				Importance: "high",
				Question:   fmt.Sprintf("Step %q failed: %s. How should the task proceed?", t.Status, firstLine(stepErr.Error(), 300)),
				Reason:     stepErr.Error(),
				Options: []domain.DecisionOption{
					{ID: "retry", Label: "Retry the step", Detail: "add guidance in the note"},
					{ID: "abort", Label: "Abort the task", Effect: "abort"},
				},
			}, t.Status, "engine"); derr != nil {
				return e.failTask(t, stepErr.Error())
			}
			return nil
		}
	}
}

// Cancel stops a running task: the step's context is cancelled (child
// process groups are killed by the sandbox), and once the loop has exited
// the task is marked interrupted with a resume point. Returns false if the
// task was not running in this process.
func (e *Engine) Cancel(id string) bool {
	e.mu.Lock()
	cancel, ok := e.cancels[id]
	e.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// MarkInterrupted is called after a cancelled RunTask returned: an active
// status becomes interrupted (never done), with the reason recorded.
func (e *Engine) MarkInterrupted(id, reason string) {
	t, err := e.Store.GetTask(id)
	if err != nil || !t.Status.Active() {
		return
	}
	t.State.ResumeStatus = t.Status
	t.Status = domain.StatusInterrupted
	if err := e.Store.SaveTask(t); err != nil {
		return
	}
	e.emit(t.ID, domain.EvTaskCancelled, map[string]any{"was": string(t.State.ResumeStatus), "reason": reason})
	runs, _ := e.Store.Runs(t.ID)
	for _, r := range runs {
		if r.Status == "running" {
			now := time.Now().UTC()
			r.Status, r.Error, r.FinishedAt = "interrupted", reason, &now
			_ = e.Store.UpdateRun(&r)
		}
	}
	_, _ = e.snapshotPacket(t)
}

func (e *Engine) failTask(t *domain.Task, reason string) error {
	t.Status = domain.StatusFailed
	t.FailureReason = reason
	if err := e.Store.SaveTask(t); err != nil {
		return err
	}
	e.emit(t.ID, domain.EvTaskFailed, map[string]any{"reason": reason})
	_, _ = e.snapshotPacket(t)
	return nil
}

func (e *Engine) setStatus(t *domain.Task, s domain.TaskStatus) error {
	prev := t.Status
	t.Status = s
	if err := e.Store.SaveTask(t); err != nil {
		return err
	}
	if prev != s {
		e.emit(t.ID, domain.EvPhaseChanged, map[string]any{"from": string(prev), "to": string(s)})
	}
	return nil
}

// RecoverInterrupted marks tasks that were mid-step when the process died.
// Call once on startup.
func (e *Engine) RecoverInterrupted() error {
	tasks, err := e.Store.ListTasks()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.Status.Active() {
			// A task whose lease is held is alive in another process — never
			// clobber it.
			unlock, err := e.Store.LockTask(t.ID)
			if err != nil {
				continue
			}
			t.State.ResumeStatus = t.Status
			t.Status = domain.StatusInterrupted
			err = e.Store.SaveTask(t)
			unlock()
			if errors.Is(err, store.ErrConflict) {
				continue // someone else moved it; not ours to touch
			}
			if err != nil {
				return err
			}
			e.emit(t.ID, domain.EvTaskInterrupted, map[string]any{"was": string(t.State.ResumeStatus)})
			// Close runs that will never finish.
			runs, _ := e.Store.Runs(t.ID)
			for _, r := range runs {
				if r.Status == "running" {
					now := time.Now().UTC()
					r.Status, r.Error, r.FinishedAt = "interrupted", "process restarted mid-run", &now
					_ = e.Store.UpdateRun(&r)
				}
			}
		}
	}
	return nil
}

// Resume returns an interrupted task to its last phase. The caller then runs
// RunTask again.
func (e *Engine) Resume(id string) (*domain.Task, error) {
	t, err := e.Store.GetTask(id)
	if err != nil {
		return nil, err
	}
	switch t.Status {
	case domain.StatusInterrupted:
		to := t.State.ResumeStatus
		if to == "" || to == domain.StatusUnderstanding {
			to = domain.StatusPending
		}
		t.State.ResumeStatus = ""
		if err := e.setStatus(t, to); err != nil {
			return nil, err
		}
		e.emit(t.ID, domain.EvTaskResumed, map[string]any{"to": string(to)})
		return t, nil
	case domain.StatusAwaitingDecision:
		return nil, errors.New("task awaits a decision; resolve it instead of resuming")
	default:
		return nil, fmt.Errorf("task is %s, nothing to resume", t.Status)
	}
}

// ---------- decisions ----------

// requireDecision persists a structured decision and pauses the task.
func (e *Engine) requireDecision(t *domain.Task, dr *domain.DecisionRequest, returnTo domain.TaskStatus, source string) error {
	d := &domain.Decision{
		ID:             domain.NewID("dec"),
		TaskID:         t.ID,
		Importance:     dr.Importance,
		Question:       dr.Question,
		Recommendation: dr.Recommendation,
		Reason:         dr.Reason,
		Options:        dr.Options,
		Status:         "open",
		ReturnTo:       returnTo,
		CreatedAt:      time.Now().UTC(),
	}
	if d.Importance == "" {
		d.Importance = "medium"
	}
	if len(d.Options) == 0 {
		d.Options = []domain.DecisionOption{
			{ID: "proceed", Label: "Proceed with the recommendation"},
			{ID: "abort", Label: "Abort the task", Effect: "abort"},
		}
	}
	if err := e.Store.CreateDecision(d); err != nil {
		return err
	}
	e.emit(t.ID, domain.EvDecisionRequired, map[string]any{
		"decision_id": d.ID, "importance": d.Importance, "question": d.Question,
		"recommendation": d.Recommendation, "reason": d.Reason, "options": d.Options, "source": source,
	})
	if err := e.setStatus(t, domain.StatusAwaitingDecision); err != nil {
		return err
	}
	_, _ = e.snapshotPacket(t)
	return nil
}

// ResolveDecision records the human's choice and unpauses the workflow.
// The chosen option's Effect decides what happens: abort fails the task,
// accept completes it as-is, extend resets the step budget, and the default
// continues at the decision's ReturnTo phase. Call RunTask afterwards to
// continue execution.
func (e *Engine) ResolveDecision(taskID, decisionID, optionID, note string) (*domain.Task, error) {
	t, err := e.Store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	// Guard: decisions are only actionable while the task is actually paused
	// on one. Without this, resolving a stale decision could resurrect a
	// done/failed task or yank a running task into another phase mid-step.
	if t.Status != domain.StatusAwaitingDecision {
		return nil, fmt.Errorf("task is %s, not awaiting a decision", t.Status)
	}
	d, err := e.Store.GetDecision(taskID, decisionID)
	if err != nil {
		return nil, err
	}
	if d.Status != "open" {
		return nil, fmt.Errorf("decision %s is already resolved", decisionID)
	}
	label, effect := optionID, ""
	if len(d.Options) > 0 {
		found := false
		for _, o := range d.Options {
			if o.ID == optionID {
				label, effect, found = o.Label, o.Effect, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown option %q; valid: %s", optionID, optionIDs(d.Options))
		}
	}
	// Back-compat for agent-emitted options without an explicit effect.
	if effect == "" && optionID == "abort" {
		effect = "abort"
	}
	now := time.Now().UTC()
	d.Status = "resolved"
	d.ChosenOption = optionID
	d.Note = note
	d.ResolvedAt = &now
	if err := e.Store.SaveDecision(d); err != nil {
		return nil, err
	}
	e.emit(taskID, domain.EvDecisionResolved, map[string]any{
		"decision_id": d.ID, "option": optionID, "effect": effect, "note": note,
	})

	guidance := fmt.Sprintf("Decision %q resolved: chose %q", d.Question, label)
	if note != "" {
		guidance += " — " + note
	}
	t.State.Notes = append(t.State.Notes, guidance)

	switch effect {
	case "abort":
		if err := e.failTask(t, "aborted by human decision"); err != nil {
			return nil, err
		}
		return t, nil
	case "accept":
		if err := e.completeTask(t); err != nil {
			return nil, err
		}
		return t, nil
	case "extend":
		t.State.Steps = 0
	}
	to := d.ReturnTo
	if to == "" {
		to = domain.StatusImplementing
	}
	if err := e.setStatus(t, to); err != nil {
		return nil, err
	}
	return t, nil
}

func optionIDs(opts []domain.DecisionOption) string {
	ids := make([]string, len(opts))
	for i, o := range opts {
		ids[i] = o.ID
	}
	return strings.Join(ids, ", ")
}

// ---------- evidence ----------

func (e *Engine) addEvidence(t *domain.Task, level domain.EvidenceLevel, claim, source, detail string) {
	ev := domain.Evidence{
		ID: domain.NewID("evd"), TaskID: t.ID, Claim: claim,
		Level: level, Source: source, Detail: detail, At: time.Now().UTC(),
	}
	if err := e.Store.AddEvidence(ev); err != nil {
		return
	}
	e.emit(t.ID, domain.EvEvidenceAdded, map[string]any{
		"level": string(level), "claim": claim, "source": source,
	})
}

// ---------- agent execution ----------

func (e *Engine) memoryRules(t *domain.Task) []string {
	if e.Mem == nil {
		return nil
	}
	scopes := make([]string, 0, len(t.Repos)*2)
	for _, r := range t.Repos {
		scopes = append(scopes, r.Name, r.Path)
	}
	rules, err := e.Mem.Relevant(scopes)
	if err != nil {
		return nil
	}
	return rules
}

// runAgent executes one agent invocation with full observability.
func (e *Engine) runAgent(ctx context.Context, t *domain.Task, role string, prompt string, rt router.Route, attempt int) (executor.Result, *domain.AgentRun, error) {
	if t.Scenario != "" {
		rt.Executor = "scenario"
		rt.Reason = "Local Pilot example: scripted agent replies (" + rt.Reason + ")"
	}
	exec, ok := e.Execs[rt.Executor]
	if !ok {
		return executor.Result{}, nil, fmt.Errorf("no executor registered under %q", rt.Executor)
	}
	run := &domain.AgentRun{
		ID: domain.NewID("run"), TaskID: t.ID, Role: role, Phase: t.Status,
		Executor: rt.Executor, Model: rt.Model, RouteReason: rt.Reason,
		Attempt: attempt, Status: "running", StartedAt: time.Now().UTC(),
	}
	if err := e.Store.AddRun(run); err != nil {
		return executor.Result{}, nil, err
	}
	e.emit(t.ID, domain.EvAgentStarted, map[string]any{
		"run_id": run.ID, "role": role, "executor": rt.Executor, "model": rt.Model, "attempt": attempt,
	})

	res, err := exec.Run(ctx, executor.Request{
		Role: role, Prompt: prompt, WorkDir: t.State.WorktreeRoot,
		Model: rt.Model, ReadOnly: roles.ReadOnly(role),
		Timeout: e.Cfg.AgentTimeout, Attempt: attempt, Scenario: t.Scenario,
	})

	now := time.Now().UTC()
	run.FinishedAt = &now
	run.DurationMS = res.DurationMS
	if run.DurationMS == 0 {
		run.DurationMS = now.Sub(run.StartedAt).Milliseconds()
	}
	run.InputTokens, run.OutputTokens = res.InputTokens, res.OutputTokens
	run.NumTurns, run.CostUSD = res.NumTurns, res.CostUSD
	if err != nil {
		run.Status = "error"
		run.Error = err.Error()
		_ = e.Store.UpdateRun(run)
		e.emit(t.ID, domain.EvAgentFailed, map[string]any{"run_id": run.ID, "role": role, "error": err.Error()})
		return res, run, err
	}
	run.Status = "ok"
	run.Summary, _ = sandbox.Redact(firstLine(res.Output, 200))
	_ = e.Store.UpdateRun(run)
	e.emit(t.ID, domain.EvAgentCompleted, map[string]any{
		"run_id": run.ID, "role": role, "model": rt.Model, "duration_ms": run.DurationMS,
		"cost_usd": run.CostUSD, "input_tokens": run.InputTokens, "output_tokens": run.OutputTokens,
		"num_turns": run.NumTurns,
	})
	return res, run, nil
}

// runParsed runs an agent and parses its structured output, retrying once
// when the reply has no valid JSON block.
func runParsed[T any](e *Engine, ctx context.Context, t *domain.Task, role, prompt string, rt router.Route, attempt int, parse func(string) (*T, error)) (*T, *domain.AgentRun, error) {
	res, run, err := e.runAgent(ctx, t, role, prompt, rt, attempt)
	if err != nil {
		return nil, run, err
	}
	out, perr := parse(res.Output)
	if perr == nil {
		return out, run, nil
	}
	e.emit(t.ID, domain.EvWarning, map[string]any{
		"warning": "agent output unparseable, retrying once", "role": role, "error": perr.Error(),
	})
	retryPrompt := prompt + "\n\nIMPORTANT: your previous reply could not be parsed (" + perr.Error() +
		"). Reply again, ending with exactly one valid JSON object in a ```json fence matching the requested schema."
	res, run, err = e.runAgent(ctx, t, role, retryPrompt, rt, attempt+1)
	if err != nil {
		return nil, run, err
	}
	out, perr = parse(res.Output)
	if perr != nil {
		return nil, run, fmt.Errorf("%s output unparseable after retry: %w", role, perr)
	}
	return out, run, nil
}

func (e *Engine) route(req router.Request) router.Route {
	rt := e.Router.Route(req)
	return rt
}

func (e *Engine) emitRoute(t *domain.Task, role string, rt router.Route) {
	e.emit(t.ID, domain.EvRouteChosen, map[string]any{
		"role": role, "executor": rt.Executor, "model": rt.Model, "reason": rt.Reason,
	})
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
