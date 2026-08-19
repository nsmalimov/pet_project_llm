package engine

import (
	"context"
	"fmt"
	"strings"

	"orchestrator/internal/domain"
	"orchestrator/internal/gitws"
	"orchestrator/internal/roles"
	"orchestrator/internal/router"
	"orchestrator/internal/verify"
)

// stepUnderstand: prepare the workspace, run the Researcher, then decide:
// implement, investigate deeper, or pause on a decision.
func (e *Engine) stepUnderstand(ctx context.Context, t *domain.Task) error {
	if err := e.setStatus(t, domain.StatusUnderstanding); err != nil {
		return err
	}
	e.emit(t.ID, domain.EvStepPlanned, map[string]any{
		"action": "understand", "reason": "new task: build understanding before touching code",
	})

	if err := e.ensureWorkspace(ctx, t); err != nil {
		return err
	}

	rt := e.route(router.Request{Role: roles.Researcher})
	e.emitRoute(t, roles.Researcher, rt)
	prompt := roles.ResearcherPrompt(roles.Input{Task: t, Rules: e.memoryRules(t)})
	out, run, err := runParsed(e, ctx, t, roles.Researcher, prompt, rt, 0, roles.ParseResearch)
	if err != nil {
		return err
	}

	t.State.Uncertainty = out.Uncertainty
	t.State.ResearchSummary = out.Summary
	t.State.KeyFiles = out.KeyFiles
	e.addEvidence(t, domain.EvidenceCodeInspected,
		"relevant code inspected and understood", run.ID, out.Summary)

	if out.DecisionRequest != nil {
		e.emit(t.ID, domain.EvStepPlanned, map[string]any{
			"action": "decide", "reason": "researcher raised a question only a human can answer",
		})
		return e.requireDecision(t, out.DecisionRequest, domain.StatusImplementing, run.ID)
	}
	if out.Uncertainty == "high" && t.State.Investigations < e.Cfg.MaxInvestigations {
		t.State.InvestigationQ = strings.Join(out.OpenQuestions, "\n")
		if t.State.InvestigationQ == "" {
			t.State.InvestigationQ = "Resolve the open uncertainty: " + out.Summary
		}
		e.emit(t.ID, domain.EvStepPlanned, map[string]any{
			"action": "investigate", "reason": "research reports high uncertainty → deep investigation before implementing",
		})
		return e.setStatus(t, domain.StatusInvestigating)
	}
	e.emit(t.ID, domain.EvStepPlanned, map[string]any{
		"action": "implement", "reason": fmt.Sprintf("understanding complete (uncertainty=%s)", out.Uncertainty),
	})
	return e.setStatus(t, domain.StatusImplementing)
}

// ensureWorkspace prepares worktrees if the task does not have them yet.
// Idempotent, so any step can be a resume point after a crash.
func (e *Engine) ensureWorkspace(ctx context.Context, t *domain.Task) error {
	if t.State.WorktreeRoot != "" {
		return nil
	}
	if err := e.WS.Prepare(ctx, t); err != nil {
		return fmt.Errorf("prepare workspace: %w", err)
	}
	if err := e.Store.SaveTask(t); err != nil {
		return err
	}
	e.emit(t.ID, domain.EvWorkspacePrepared, map[string]any{
		"worktree_root": t.State.WorktreeRoot, "branch": t.State.Branch, "base_shas": t.State.BaseSHAs,
	})
	return nil
}

// stepInvestigate: deep-dive Researcher run answering a specific question.
func (e *Engine) stepInvestigate(ctx context.Context, t *domain.Task) error {
	if err := e.ensureWorkspace(ctx, t); err != nil {
		return err
	}
	q := t.State.InvestigationQ
	if q == "" {
		q = "Resolve the remaining uncertainty preventing implementation."
	}
	t.State.Investigations++
	rt := e.route(router.Request{Role: roles.Researcher, Deep: true})
	e.emitRoute(t, roles.Researcher, rt)
	prompt := roles.ResearcherPrompt(roles.Input{
		Task: t, Rules: e.memoryRules(t), InvestigationQuestion: q,
	})
	out, run, err := runParsed(e, ctx, t, roles.Researcher, prompt, rt, t.State.Investigations, roles.ParseResearch)
	if err != nil {
		return err
	}

	t.State.ResearchSummary += "\n\nInvestigation findings (" + firstLine(q, 120) + "):\n" + out.Summary
	t.State.Uncertainty = out.Uncertainty
	t.State.InvestigationQ = ""
	if len(out.KeyFiles) > 0 {
		t.State.KeyFiles = mergeUnique(t.State.KeyFiles, out.KeyFiles)
	}
	e.addEvidence(t, domain.EvidenceCodeInspected, "investigation: "+firstLine(q, 120), run.ID, out.Summary)

	if out.DecisionRequest != nil {
		return e.requireDecision(t, out.DecisionRequest, domain.StatusImplementing, run.ID)
	}
	e.emit(t.ID, domain.EvStepPlanned, map[string]any{
		"action": "implement", "reason": "investigation complete → back to implementation",
	})
	return e.setStatus(t, domain.StatusImplementing)
}

// stepImplement: run the Developer with everything relevant (research,
// failing tests, review findings), then verify — or branch to investigation /
// decision if the developer is blocked.
func (e *Engine) stepImplement(ctx context.Context, t *domain.Task) error {
	if err := e.ensureWorkspace(ctx, t); err != nil {
		return err
	}
	rt := e.route(router.Request{
		Role:        roles.Developer,
		Uncertainty: t.State.Uncertainty,
		Attempt:     t.State.FixAttempts,
	})
	e.emitRoute(t, roles.Developer, rt)
	prompt := roles.DeveloperPrompt(roles.Input{
		Task: t, Rules: e.memoryRules(t),
		ResearchSummary: t.State.ResearchSummary,
		KeyFiles:        t.State.KeyFiles,
		TestFailures:    failedOnly(t.State.LastTests),
		ReviewFindings:  t.State.ReviewFindings,
	})
	out, run, err := runParsed(e, ctx, t, roles.Developer, prompt, rt, t.State.ImplementAttempts, roles.ParseDevelop)
	if err != nil {
		return err
	}
	t.State.ImplementAttempts++
	t.State.DeveloperSummary = out.Summary
	t.State.AuthorModel = rt.Model

	_, files, derr := e.WS.Diff(ctx, t)
	if derr != nil {
		return fmt.Errorf("compute diff: %w", derr)
	}
	t.State.ChangedFiles = files
	e.emit(t.ID, domain.EvFilesChanged, map[string]any{"files": files, "count": len(files)})

	switch out.Status {
	case "completed":
		// Findings are addressed; the reviewer will re-check the new diff.
		t.State.ReviewFindings = nil
		if len(files) == 0 {
			e.emit(t.ID, domain.EvWarning, map[string]any{
				"warning": "developer reported completed but no files changed",
			})
		} else {
			e.addEvidence(t, domain.EvidenceImplemented,
				"a concrete change implementing the task exists", run.ID,
				fmt.Sprintf("%d file(s) changed", len(files)))
		}
		e.emit(t.ID, domain.EvStepPlanned, map[string]any{
			"action": "verify", "reason": "implementation complete → run real verification",
		})
		return e.setStatus(t, domain.StatusVerifying)

	case "blocked", "uncertain":
		if out.DecisionRequest != nil {
			e.emit(t.ID, domain.EvStepPlanned, map[string]any{
				"action": "decide", "reason": "developer " + out.Status + " and raised a human decision",
			})
			return e.requireDecision(t, out.DecisionRequest, domain.StatusImplementing, run.ID)
		}
		if t.State.Investigations < e.Cfg.MaxInvestigations {
			t.State.InvestigationQ = out.Notes
			if t.State.InvestigationQ == "" {
				t.State.InvestigationQ = "Developer was " + out.Status + ": " + out.Summary
			}
			e.emit(t.ID, domain.EvStepPlanned, map[string]any{
				"action": "investigate", "reason": "developer " + out.Status + " → deep investigation",
			})
			return e.setStatus(t, domain.StatusInvestigating)
		}
		return e.requireDecision(t, &domain.DecisionRequest{
			Importance: "high",
			Question:   "Developer is repeatedly blocked: " + firstLine(out.Summary, 200) + ". How should the task proceed?",
			Reason:     out.Notes,
			Options: []domain.DecisionOption{
				{ID: "retry", Label: "Retry implementation", Detail: "add guidance in the note"},
				{ID: "abort", Label: "Abort the task", Effect: "abort"},
			},
		}, domain.StatusImplementing, run.ID)
	}
	return fmt.Errorf("unreachable developer status %q", out.Status)
}

// stepVerify: run real test commands in every worktree. This is the Tester
// role — implemented as command execution, not an LLM.
func (e *Engine) stepVerify(ctx context.Context, t *domain.Task) error {
	if err := e.ensureWorkspace(ctx, t); err != nil {
		return err
	}
	rt := e.route(router.Request{Role: roles.Tester})
	e.emitRoute(t, roles.Tester, rt)

	var results []domain.TestResult
	ran := 0
	for _, r := range t.Repos {
		dir := gitws.RepoDir(t, r)
		cmd := t.TestCommand
		if cmd == "" {
			cmd = verify.DetectCommand(dir)
		}
		if cmd == "" {
			continue
		}
		ran++
		e.emit(t.ID, domain.EvTestsStarted, map[string]any{"repo": r.Name, "command": cmd})
		res := verify.Run(ctx, r.Name, dir, cmd, e.Cfg.TestTimeout)
		if ctx.Err() != nil {
			// Cancellation is not a test failure; don't burn a fix attempt.
			return ctx.Err()
		}
		results = append(results, res)
		if res.Passed {
			e.emit(t.ID, domain.EvTestsPassed, map[string]any{"repo": r.Name, "command": cmd})
		} else {
			e.emit(t.ID, domain.EvTestsFailed, map[string]any{
				"repo": r.Name, "command": cmd, "exit_code": res.ExitCode, "output_tail": tailStr(res.OutputTail, 1500),
			})
		}
	}
	t.State.LastTests = results

	if ran == 0 {
		e.emit(t.ID, domain.EvTestsSkipped, map[string]any{
			"reason": "no test command detected in any repo",
		})
		e.emit(t.ID, domain.EvStepPlanned, map[string]any{
			"action": "review", "reason": "no automated verification available → rely on independent review",
		})
		return e.setStatus(t, domain.StatusReviewing)
	}
	if allPassed(results) {
		e.addEvidence(t, domain.EvidenceTested,
			"automated tests pass after the change", "tester", summarizeTests(results))
		e.emit(t.ID, domain.EvStepPlanned, map[string]any{
			"action": "review", "reason": "verification passed → independent review",
		})
		return e.setStatus(t, domain.StatusReviewing)
	}

	t.State.FixAttempts++
	if t.State.FixAttempts > e.Cfg.MaxFixAttempts {
		e.emit(t.ID, domain.EvStepPlanned, map[string]any{
			"action": "decide", "reason": fmt.Sprintf("tests still failing after %d fix attempts", t.State.FixAttempts-1),
		})
		return e.requireDecision(t, &domain.DecisionRequest{
			Importance: "high",
			Question:   fmt.Sprintf("Tests keep failing after %d fix attempts. How should the task proceed?", t.State.FixAttempts-1),
			Reason:     summarizeTests(results),
			Options: []domain.DecisionOption{
				{ID: "retry", Label: "Keep trying", Detail: "add guidance in the note"},
				{ID: "abort", Label: "Abort the task", Effect: "abort"},
			},
		}, domain.StatusImplementing, "tester")
	}
	e.emit(t.ID, domain.EvStepPlanned, map[string]any{
		"action": "implement", "reason": fmt.Sprintf("tests failed → back to implementation (fix attempt %d/%d)", t.State.FixAttempts, e.Cfg.MaxFixAttempts),
	})
	return e.setStatus(t, domain.StatusImplementing)
}

// stepReview: independent review of the diff against the requirements. The
// reviewer never sees the developer's reasoning and runs read-only, routed to
// a model different from the author's.
func (e *Engine) stepReview(ctx context.Context, t *domain.Task) error {
	if err := e.ensureWorkspace(ctx, t); err != nil {
		return err
	}
	diff, files, err := e.WS.Diff(ctx, t)
	if err != nil {
		return fmt.Errorf("compute diff: %w", err)
	}
	if strings.TrimSpace(diff) == "" {
		return e.requireDecision(t, &domain.DecisionRequest{
			Importance: "high",
			Question:   "The task reached review with an empty diff — no changes were produced. How should it proceed?",
			Reason:     "developer reported completion but the worktrees contain no changes",
			Options: []domain.DecisionOption{
				{ID: "retry", Label: "Send back to implementation"},
				{ID: "abort", Label: "Abort the task", Effect: "abort"},
			},
		}, domain.StatusImplementing, "engine")
	}

	// Cap the inlined diff; the reviewer can read full files in the worktree.
	const maxDiffBytes = 150_000
	if len(diff) > maxDiffBytes {
		e.emit(t.ID, domain.EvWarning, map[string]any{
			"warning": "diff truncated for review prompt", "bytes": len(diff),
		})
		diff = diff[:maxDiffBytes] +
			"\n\n[diff truncated — read the changed files in the worktree for full context]"
	}

	e.emit(t.ID, domain.EvReviewStarted, map[string]any{"files": files})
	rt := e.route(router.Request{
		Role: roles.Reviewer, NeedIndependence: true, AuthorModel: t.State.AuthorModel,
	})
	e.emitRoute(t, roles.Reviewer, rt)
	prompt := roles.ReviewerPrompt(roles.Input{
		Task: t, Rules: e.memoryRules(t), Diff: diff, ChangedFiles: files,
	})
	out, run, err := runParsed(e, ctx, t, roles.Reviewer, prompt, rt, t.State.ReviewRounds, roles.ParseReview)
	if err != nil {
		return err
	}

	for _, f := range out.Findings {
		e.emit(t.ID, domain.EvReviewFinding, map[string]any{
			"severity": f.Severity, "file": f.File, "issue": f.Issue,
		})
	}
	e.emit(t.ID, domain.EvReviewCompleted, map[string]any{
		"verdict": out.Verdict, "summary": out.Summary, "findings": len(out.Findings),
	})

	if out.Verdict == "approve" {
		e.addEvidence(t, domain.EvidenceReviewed,
			"independent reviewer confirmed the change satisfies the requirements", run.ID, out.Summary)
		return e.completeTask(t)
	}

	t.State.ReviewRounds++
	t.State.ReviewFindings = out.Findings
	if t.State.ReviewRounds > e.Cfg.MaxReviewRounds {
		return e.requireDecision(t, &domain.DecisionRequest{
			Importance: "high",
			Question:   fmt.Sprintf("Review still requests changes after %d rounds. How should the task proceed?", t.State.ReviewRounds-1),
			Reason:     out.Summary,
			Options: []domain.DecisionOption{
				{ID: "retry", Label: "Another implementation round"},
				{ID: "accept", Label: "Accept as-is (overrides the reviewer)", Effect: "accept"},
				{ID: "abort", Label: "Abort the task", Effect: "abort"},
			},
		}, domain.StatusImplementing, run.ID)
	}
	e.emit(t.ID, domain.EvStepPlanned, map[string]any{
		"action": "implement", "reason": fmt.Sprintf("review requested changes (round %d/%d) → back to implementation", t.State.ReviewRounds, e.Cfg.MaxReviewRounds),
	})
	return e.setStatus(t, domain.StatusImplementing)
}

func (e *Engine) completeTask(t *domain.Task) error {
	if err := e.setStatus(t, domain.StatusDone); err != nil {
		return err
	}
	evidence, _ := e.Store.EvidenceList(t.ID)
	best := domain.EvidenceAssumed
	for _, ev := range evidence {
		if ev.Level.Rank() > best.Rank() {
			best = ev.Level
		}
	}
	runs, _ := e.Store.Runs(t.ID)
	var cost float64
	var inTok, outTok int
	for _, r := range runs {
		cost += r.CostUSD
		inTok += r.InputTokens
		outTok += r.OutputTokens
	}
	e.emit(t.ID, domain.EvTaskCompleted, map[string]any{
		"confidence":     string(best),
		"changed_files":  t.State.ChangedFiles,
		"branch":         t.State.Branch,
		"worktree_root":  t.State.WorktreeRoot,
		"agent_runs":     len(runs),
		"total_cost_usd": cost,
		"input_tokens":   inTok,
		"output_tokens":  outTok,
	})
	return nil
}

// ---------- helpers ----------

func failedOnly(results []domain.TestResult) []domain.TestResult {
	var out []domain.TestResult
	for _, r := range results {
		if !r.Passed {
			out = append(out, r)
		}
	}
	return out
}

func allPassed(results []domain.TestResult) bool {
	for _, r := range results {
		if !r.Passed {
			return false
		}
	}
	return len(results) > 0
}

func summarizeTests(results []domain.TestResult) string {
	var sb strings.Builder
	for _, r := range results {
		state := "PASS"
		if !r.Passed {
			state = "FAIL"
		}
		fmt.Fprintf(&sb, "[%s] %s: %s\n", state, r.Repo, r.Command)
	}
	return strings.TrimSpace(sb.String())
}

func mergeUnique(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(a, b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
