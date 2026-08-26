// Package proof derives a change-assurance packet (claims → evidence →
// verdict) strictly from persisted artifacts.
//
// Invariant: SUPPORTED never means "an agent thinks this is correct". It means
// the current persisted evidence satisfies the explicit policy of that claim
// and is bound to the exact source state (worktree HEAD per repo) the packet
// describes. Missing evidence → INSUFFICIENT. Evidence observed on a source
// state that is no longer current → STALE. Evidence that says the opposite →
// CONTRADICTED (and the packet is BLOCKED).
//
// Build is a pure function so it can be unit-tested and re-run at any time;
// identical inputs yield an identical fingerprint.
package proof

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"orchestrator/internal/domain"
)

// Input is everything the builder may look at. All of it is persisted state
// plus the current source state observed by the caller.
type Input struct {
	Task      *domain.Task
	Artifacts []domain.Artifact
	Evidence  []domain.Evidence
	Runs      []domain.AgentRun
	Decisions []domain.Decision

	// CurrentSHAs is the worktree HEAD per repo right now; CurrentDirty says
	// uncommitted changes exist. SourceUnknown means the caller could not
	// observe it (no worktree) — then nothing can be current.
	CurrentSHAs   map[string]string
	CurrentDirty  bool
	SourceUnknown bool

	// FileExists lets the builder check that a root-cause location really
	// exists in the worktree; nil disables the check.
	FileExists func(repoRelPath string) bool
	// IntegrationConfigured says the repository policy defines an
	// integration check; then the claim is core and a missing/failed run
	// counts against the packet.
	IntegrationConfigured bool
}

// RelativeInsideWorktree rejects agent-supplied paths that leave the worktree.
var RelativeInsideWorktree = func(root, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes worktree")
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

// WorktreeFileExists returns a FileExists func bound to a task's worktree.
func WorktreeFileExists(t *domain.Task) func(string) bool {
	return func(p string) bool {
		if t.State.WorktreeRoot == "" {
			return false
		}
		full, err := RelativeInsideWorktree(t.State.WorktreeRoot, p)
		if err != nil {
			return false
		}
		_, err = os.Stat(full)
		return err == nil
	}
}

// builder holds the indexed view of one Build call.
type builder struct {
	in        Input
	t         *domain.Task
	p         domain.Packet
	ignored   int
	baselines []domain.Artifact // on the base SHAs
	staleBase []domain.Artifact
	tests     []domain.Artifact // test_run on the current state
	staleRuns []domain.Artifact
	replays   []domain.Artifact // original_tests_run on the current state
	integ     []domain.Artifact // integration_run on the current state
	integBase []domain.Artifact // integration_run on the base
	integOld  []domain.Artifact // integration_run elsewhere (stale)
	diff      *domain.Artifact  // last diff, any state
	review    *domain.Artifact  // last review, any state
	rootCause *domain.Artifact
}

// Build derives a packet. Version is assigned by the caller.
func Build(in Input) domain.Packet {
	b := &builder{in: in, t: in.Task}
	b.p = domain.Packet{
		TaskID: in.Task.ID, TaskStatus: in.Task.Status,
		Gaps: []string{}, Risks: []domain.Risk{}, Contradictions: []string{},
		Source: domain.SourceState{BaseSHAs: in.Task.State.BaseSHAs, HeadSHAs: in.CurrentSHAs, Dirty: in.CurrentDirty},
	}
	b.index()
	b.change()
	b.p.Claims = []domain.Claim{
		b.claimReproduced(), b.claimRootCause(), b.claimChangeVerified(),
		b.claimChallenge(), b.claimIntegration(), b.claimCrossService(),
	}
	b.gapsAndRisks()
	b.p.Verdict, b.p.VerdictWhy = b.verdict()

	best := domain.EvidenceAssumed
	for _, ev := range in.Evidence {
		if ev.Level.Rank() > best.Rank() {
			best = ev.Level
		}
	}
	b.p.Confidence = string(best)
	b.p.Fingerprint = fingerprint(b.p)
	return b.p
}

// ---------- source-state binding ----------

func sameSHAs(a, b map[string]string) bool {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// current reports whether an artifact was observed on the current state.
func (b *builder) current(a domain.Artifact) bool {
	if b.in.SourceUnknown || b.in.CurrentDirty || a.SourceDirty {
		return false
	}
	return sameSHAs(a.SourceSHAs, b.in.CurrentSHAs)
}

// onBase reports whether an artifact was observed on the untouched base.
func (b *builder) onBase(a domain.Artifact) bool {
	return !a.SourceDirty && sameSHAs(a.SourceSHAs, b.t.State.BaseSHAs)
}

func (b *builder) index() {
	for i := range b.in.Artifacts {
		if a := &b.in.Artifacts[i]; a.TestsParsed && a.Tests == nil {
			a.Tests = []domain.TestCase{} // omitempty dropped the empty list on disk
		}
		a := b.in.Artifacts[i]
		// Provenance: artifacts of another task or workspace are never used.
		if a.TaskID != b.t.ID || (a.WorktreeRoot != "" && b.t.State.WorktreeRoot != "" && a.WorktreeRoot != b.t.State.WorktreeRoot) {
			b.ignored++
			continue
		}
		switch a.Kind {
		case domain.ArtBaselineRun:
			if b.onBase(a) {
				b.baselines = append(b.baselines, a)
			} else {
				b.staleBase = append(b.staleBase, a)
			}
		case domain.ArtTestRun:
			if b.current(a) {
				b.tests = append(b.tests, a)
			} else {
				b.staleRuns = append(b.staleRuns, a)
			}
		case domain.ArtOriginalTestsRun:
			if b.current(a) {
				b.replays = append(b.replays, a)
			}
		case domain.ArtIntegrationRun:
			switch {
			case b.current(a):
				b.integ = append(b.integ, a)
			case b.onBase(a):
				b.integBase = append(b.integBase, a)
			default:
				b.integOld = append(b.integOld, a)
			}
		case domain.ArtDiff:
			b.diff = &b.in.Artifacts[i]
		case domain.ArtReview:
			b.review = &b.in.Artifacts[i]
		case domain.ArtRootCause:
			b.rootCause = &b.in.Artifacts[i]
		}
	}
}

func (b *builder) change() {
	b.p.Change.Files = []string{}
	if b.diff == nil {
		return
	}
	b.p.Change = domain.ChangeSummary{
		Files: b.diff.Files, Branch: b.diff.Branch, Commits: b.diff.Commits,
		AuthorModel: b.diff.Model, DiffArtifact: b.diff.ID,
	}
	for _, f := range b.diff.Files {
		if IsTestFile(f) {
			b.p.Change.TestFiles = append(b.p.Change.TestFiles, f)
		}
	}
}

// ---------- claim policies (explicit, human-readable, part of the packet) ----------

const (
	PolicyReproduced = "SUPPORTED only if a baseline_run artifact of the reproduction command, observed on the recorded base SHAs, exited non-zero with at least one failing test case (when the runner is parseable) and did not time out; a passing baseline is CONTRADICTED."
	PolicyRootCause  = "SUPPORTED only as a cross-check: the researcher named a file that exists, the diff modifies that file, and the reproduction flipped fail→pass on the current state. Never a causal proof."
	PolicyVerified   = "SUPPORTED only if the diff artifact and every test_run artifact are on the current source state (no dirty tree), all runs passed without timeout, repeated runs agree, at least one test executed, output was not truncated, every test that failed on the baseline was observed passing, and — if the author modified test files — the original tests replayed against the change also pass."
	PolicyChallenge  = "SUPPORTED only if a review artifact on the current state has verdict=approve, no counterexample, no high-severity finding, a model different from the author's, and a non-empty list of checked aspects. Means 'no counterexample found', not 'correct'."
	PolicyIntegrate  = "SUPPORTED only if the repository policy configures an integration check and an integration_run artifact on the current state shows every configured HTTP check passing against the service started from the worktree; a failing check is CONTRADICTED; not configured is INSUFFICIENT."
	PolicyCross      = "SUPPORTED only if a cross-repository verification artifact exists. None can be produced in this build, so this claim is always INSUFFICIENT."
)

// ---------- claims ----------

// reproCommand is the command whose baseline failure counts as reproduction:
// the narrow repro command when the task has one, else the test command.
func (b *builder) reproArtifacts(list []domain.Artifact) []domain.Artifact {
	var out []domain.Artifact
	for _, a := range list {
		if b.t.ReproCommand != "" {
			if a.Command == b.t.ReproCommand {
				out = append(out, a)
			}
		} else if !a.Narrow {
			out = append(out, a)
		}
	}
	return out
}

func (b *builder) claimReproduced() domain.Claim {
	c := domain.Claim{Type: domain.ClaimProblemReproduced, Title: "Problem reproduced", Core: b.t.Kind == domain.KindBugfix, Policy: PolicyReproduced}
	if b.t.Kind != domain.KindBugfix {
		c.Status = domain.ClaimInsufficient
		c.Statement = "Not a bugfix task; no failing behaviour was expected on the baseline."
		c.Reason = "task kind is " + string(b.t.Kind)
		c.ArtifactIDs = ids(b.baselines)
		return c
	}
	repro := b.reproArtifacts(b.baselines)
	if len(repro) == 0 {
		c.Status = domain.ClaimInsufficient
		if len(b.staleBase) > 0 {
			c.Status = domain.ClaimStale
			c.Statement = "Baseline runs exist but were not observed on the recorded base revision."
			c.Reason = "artifact source state does not match the base SHA"
			c.ArtifactIDs = ids(b.staleBase)
		} else {
			c.Statement = "No baseline run of the reproduction command exists."
			c.Reason = "the reproduction step did not run (no test/repro command detected) or ran a different command"
		}
		c.Gap = "run the failing test on the unchanged code"
		return c
	}
	c.ArtifactIDs = ids(repro)
	// Prefer a failing baseline (multi-repo: the bug lives in one repo).
	a := repro[0]
	for _, x := range repro {
		if x.Passed != nil && !*x.Passed {
			a = x
			break
		}
	}
	if a.Passed != nil && *a.Passed && a.Tests != nil && len(a.Tests) == 0 {
		c.Status = domain.ClaimInsufficient
		c.Statement = fmt.Sprintf("`%s` executed no test on the unchanged code.", a.Command)
		c.Reason = "there is no test that exercises the reported bug"
		c.Gap = "a test that fails before the change"
		return c
	}
	if a.Passed != nil && *a.Passed {
		if b.t.ReproCommand == "" {
			// No explicit reproduction was configured; a green suite before
			// the change is a missing proof, not a contradiction (the fix may
			// add the regression test itself).
			c.Status = domain.ClaimInsufficient
			c.Statement = fmt.Sprintf("`%s` passes on the unchanged code; no test reproduces the reported problem.", a.Command)
			c.Reason = "no reproduction command was configured, so nothing on the baseline demonstrates the bug"
			c.Gap = "a repro command / test that fails before the change"
			return c
		}
		c.Status = domain.ClaimContradicted
		c.Statement = fmt.Sprintf("`%s` PASSES on the unchanged code.", a.Command)
		c.Reason = "the configured reproduction command does not exercise the reported bug, so a later pass proves nothing about it"
		c.Gap = "a repro command / test that fails before the change"
		return c
	}
	if a.Truncated {
		c.Status = domain.ClaimInsufficient
		c.Statement = fmt.Sprintf("`%s` failed on the unchanged code but its output was truncated.", a.Command)
		c.Reason = "incomplete artifact"
		c.Gap = "a baseline run whose full output fits the cap"
		return c
	}
	c.Scope = fmt.Sprintf("repo %s, command `%s`", a.Repo, a.Command)
	if a.TimedOut {
		c.Status = domain.ClaimInsufficient
		c.Statement = fmt.Sprintf("`%s` timed out on the unchanged code.", a.Command)
		c.Reason = "a timeout is not a reproduced failure"
		c.Gap = "a baseline run that fails for the reported reason"
		return c
	}
	failed := testsWith(a.Tests, "fail")
	if a.Tests != nil && len(failed) == 0 {
		c.Status = domain.ClaimInsufficient
		c.Statement = fmt.Sprintf("`%s` exits %d on the unchanged code but no test reported a failure.", a.Command, a.ExitCode)
		c.Reason = "the runner failed without executing a failing test (build error, missing package, wrong -run pattern?)"
		c.Gap = "a baseline run where the reproduction test itself fails"
		return c
	}
	c.Status = domain.ClaimSupported
	c.Statement = fmt.Sprintf("`%s` fails on the unchanged code (exit %d).", a.Command, a.ExitCode)
	if a.Tests == nil {
		c.Reason = "baseline run on base revision; individual test names not parseable for this runner"
	} else {
		c.Reason = "baseline run on base revision; failing: " + joinMax(failed, 5)
	}
	return c
}

func (b *builder) claimRootCause() domain.Claim {
	c := domain.Claim{Type: domain.ClaimRootCauseSupported, Title: "Root cause supported", Core: b.t.Kind == domain.KindBugfix, Policy: PolicyRootCause}
	rc := b.rootCause
	if rc == nil || rc.RootCause == nil || strings.TrimSpace(rc.RootCause.Statement) == "" {
		c.Status = domain.ClaimInsufficient
		c.Statement = "No root-cause hypothesis was recorded."
		c.Reason = "the researcher did not locate a defect"
		c.Gap = "a hypothesis naming the defective code, cross-checked by the fix and the tests"
		return c
	}
	c.ArtifactIDs = append(c.ArtifactIDs, rc.ID)
	loc := rc.RootCause.File
	if rc.RootCause.Line > 0 {
		loc = fmt.Sprintf("%s:%d", loc, rc.RootCause.Line)
	}
	c.Statement = rc.RootCause.Statement
	if loc != "" {
		c.Statement += " (" + loc + ")"
	}
	var problems []string
	if rc.RootCause.File == "" {
		problems = append(problems, "hypothesis names no file")
	} else if b.in.FileExists != nil && !b.in.FileExists(rc.RootCause.File) {
		problems = append(problems, "named file does not exist in the worktree")
	}
	touched := false
	if b.diff != nil {
		c.ArtifactIDs = append(c.ArtifactIDs, b.diff.ID)
		for _, f := range b.diff.Files {
			if f == rc.RootCause.File {
				touched = true
			}
		}
	}
	if rc.RootCause.File != "" && !touched {
		problems = append(problems, "the fix does not modify the named file")
	}
	flip, flipIDs := b.baselineFailedThenPassed()
	if !flip {
		problems = append(problems, "no baseline-fail → after-pass pair on the current state confirms the location")
	} else {
		c.ArtifactIDs = append(c.ArtifactIDs, flipIDs...)
	}
	if len(problems) == 0 {
		c.Status = domain.ClaimSupported
		c.Reason = "hypothesis names an existing file, the fix modifies exactly that file, and the reproduction flipped fail → pass on the current state (cross-check, not a causal proof)"
		return c
	}
	c.Status = domain.ClaimInsufficient
	c.Reason = "hypothesis is agent-reported and not cross-checked: " + strings.Join(problems, "; ")
	c.Gap = strings.Join(problems, "; ")
	return c
}

// baselineFailedThenPassed: the reproduction command failed on base and the
// same command passes on the current state — and, when test names are
// known, every test that failed on base ran and passed now.
func (b *builder) baselineFailedThenPassed() (bool, []string) {
	var idsOut []string
	for _, base := range b.reproArtifacts(b.baselines) {
		if base.Passed == nil || *base.Passed || base.TimedOut {
			continue
		}
		failed := testsWith(base.Tests, "fail")
		if base.Tests != nil && len(failed) == 0 {
			continue
		}
		for _, after := range b.tests {
			if after.Command != base.Command || after.Repo != base.Repo || after.Passed == nil || !*after.Passed {
				continue
			}
			if base.Tests != nil && !allPassedNow(failed, after.Tests) {
				continue
			}
			idsOut = append(idsOut, base.ID, after.ID)
			return true, idsOut
		}
	}
	return false, nil
}

func (b *builder) claimChangeVerified() domain.Claim {
	c := domain.Claim{Type: domain.ClaimChangeVerified, Title: "Change verified", Core: true, Policy: PolicyVerified}
	if b.diff == nil || len(b.diff.Files) == 0 {
		c.Status = domain.ClaimInsufficient
		c.Statement = "No change exists."
		c.Reason = "no diff artifact was recorded"
		c.Gap = "an implementation"
		return c
	}
	c.ArtifactIDs = append(c.ArtifactIDs, b.diff.ID)
	if b.in.CurrentDirty {
		c.Status = domain.ClaimStale
		c.Statement = "The worktree has uncommitted changes; no test run describes the code as it is now."
		c.Reason = "source state is dirty"
		c.Gap = "commit and re-run verification on the current code"
		return c
	}
	if !b.current(*b.diff) {
		c.Status = domain.ClaimStale
		c.Statement = "The recorded change does not match the current worktree state."
		c.Reason = fmt.Sprintf("diff observed on %s, worktree is at %s", short(b.diff.SourceSHAs), short(b.in.CurrentSHAs))
		c.Gap = "re-run implementation/verification on the current state"
		return c
	}
	if len(b.tests) == 0 {
		if len(b.staleRuns) > 0 {
			c.Status = domain.ClaimStale
			c.Statement = "Tests ran, but on an earlier revision of the change."
			c.Reason = "the code changed after the last verification"
			c.ArtifactIDs = append(c.ArtifactIDs, ids(b.staleRuns)...)
		} else {
			c.Status = domain.ClaimInsufficient
			c.Statement = "No tests ran after the change."
			c.Reason = "no test_run artifact on the current state"
		}
		c.Gap = "run the test suite on the current code"
		return c
	}
	c.ArtifactIDs = append(c.ArtifactIDs, ids(b.tests)...)

	// Any failure, timeout or disagreement between repeats contradicts.
	byCmd := map[string][]domain.Artifact{}
	for _, a := range b.tests {
		byCmd[a.Repo+"|"+a.Command] = append(byCmd[a.Repo+"|"+a.Command], a)
	}
	var failing []domain.Artifact
	var flaky []string
	for _, group := range byCmd {
		p, f := 0, 0
		for _, a := range group {
			if a.Passed != nil && *a.Passed && !a.TimedOut {
				p++
			} else {
				f++
				failing = append(failing, a)
			}
		}
		if p > 0 && f > 0 {
			flaky = append(flaky, group[0].Command)
		}
	}
	if len(flaky) > 0 {
		c.Status = domain.ClaimContradicted
		c.Statement = fmt.Sprintf("`%s` both passed and failed on the same code — the verification is flaky.", flaky[0])
		c.Reason = "repeated runs of the same command disagree"
		c.Gap = "a deterministic test"
		return c
	}
	if len(failing) > 0 {
		a := failing[0]
		c.Status = domain.ClaimContradicted
		if a.TimedOut {
			c.Statement = fmt.Sprintf("`%s` timed out after the change.", a.Command)
			c.Reason = "a timeout is a failed verification, not a pass"
		} else {
			c.Statement = fmt.Sprintf("`%s` FAILS after the change (exit %d).", a.Command, a.ExitCode)
			if f := testsWith(a.Tests, "fail"); len(f) > 0 {
				c.Reason = "failing after the change: " + joinMax(f, 5)
			} else {
				c.Reason = "a test run on the current state failed"
			}
		}
		c.Gap = "a passing run"
		return c
	}
	// Completeness: a capped output cannot prove which tests ran.
	for _, a := range b.tests {
		if a.Truncated {
			c.Status = domain.ClaimInsufficient
			c.Statement = fmt.Sprintf("`%s` passed but its output was truncated by the artifact cap.", a.Command)
			c.Reason = "incomplete artifact: per-test results cannot be trusted"
			c.Gap = "a run whose full output fits the cap (narrow the command)"
			return c
		}
	}
	// Every run passed. Did anything actually execute?
	for _, a := range b.tests {
		if a.Tests != nil && len(a.Tests) == 0 {
			c.Status = domain.ClaimInsufficient
			c.Statement = fmt.Sprintf("`%s` exits 0 but executed no test.", a.Command)
			c.Reason = "exit 0 without any test case reported (filtered out, skipped, deleted?)"
			c.Gap = "a run in which the relevant tests actually execute"
			return c
		}
	}
	cmds := make([]string, 0, len(byCmd))
	for _, group := range byCmd {
		cmds = append(cmds, "`"+group[0].Command+"`")
	}
	sort.Strings(cmds)
	c.Scope = "commands " + strings.Join(cmds, ", ") + " in the task worktree(s); nothing outside them"

	// Author-modified tests: the original tests must still pass on the fix.
	if len(b.p.Change.TestFiles) > 0 {
		if len(b.replays) == 0 {
			c.Status = domain.ClaimInsufficient
			c.Statement = "Tests pass, but the author modified test files and the original tests were not replayed against the change."
			c.Reason = "no original_tests_run artifact on the current state"
			c.Gap = "replay the pre-change tests against the changed code"
			return c
		}
		c.ArtifactIDs = append(c.ArtifactIDs, ids(b.replays)...)
		for _, r := range b.replays {
			if r.Passed == nil || !*r.Passed {
				c.Status = domain.ClaimContradicted
				c.Statement = fmt.Sprintf("The ORIGINAL tests fail against the changed code (`%s`, exit %d); only the author's rewritten tests pass.", r.Command, r.ExitCode)
				if f := testsWith(r.Tests, "fail"); len(f) > 0 {
					c.Reason = "original tests failing: " + joinMax(f, 5)
				} else {
					c.Reason = "the change satisfies its own tests but not the tests it replaced"
				}
				c.Gap = "either the original tests were wrong (say so explicitly) or the fix is"
				return c
			}
		}
	}

	if b.t.Kind == domain.KindBugfix {
		flip, flipIDs := b.baselineFailedThenPassed()
		if flip {
			c.ArtifactIDs = append(c.ArtifactIDs, flipIDs...)
			c.Status = domain.ClaimSupported
			c.Statement = "The reproduction that failed on the baseline passes on the current code; " + strings.Join(cmds, ", ") + " pass."
			c.Reason = "same command, base → current: exit ≠ 0 → 0" + b.flipDetail()
			return c
		}
		c.Status = domain.ClaimInsufficient
		c.Statement = strings.Join(cmds, ", ") + " pass on the current code."
		repro := b.reproArtifacts(b.baselines)
		switch {
		case len(repro) == 0:
			c.Reason = "tests pass, but no baseline failure of the same command proves they exercise the bug"
			c.Gap = "a baseline failure of the reproduction command"
		default:
			c.Reason = "the tests that failed on the baseline did not all run and pass on the current code (" + b.missingReproTests() + ")"
			c.Gap = "the baseline-failing tests executing and passing now"
		}
		return c
	}
	c.Status = domain.ClaimSupported
	c.Statement = strings.Join(cmds, ", ") + " pass on the current code."
	c.Reason = "real test run on the current source state"
	for _, a := range b.tests {
		if !a.TestsParsed {
			c.Reason += "; test identity unverifiable for this runner (command-level pass only)"
			break
		}
	}
	return c
}

func (b *builder) flipDetail() string {
	for _, base := range b.reproArtifacts(b.baselines) {
		if f := testsWith(base.Tests, "fail"); len(f) > 0 {
			return "; " + joinMax(f, 5) + " now pass"
		}
	}
	return ""
}

func (b *builder) missingReproTests() string {
	for _, base := range b.reproArtifacts(b.baselines) {
		failed := testsWith(base.Tests, "fail")
		if len(failed) == 0 {
			continue
		}
		var missing []string
		for _, name := range failed {
			ok := false
			for _, after := range b.tests {
				if after.Command == base.Command && has(after.Tests, name, "pass") {
					ok = true
				}
			}
			if !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return "not observed passing: " + joinMax(missing, 5)
		}
	}
	return "no matching after-change run"
}

func (b *builder) claimChallenge() domain.Claim {
	c := domain.Claim{Type: domain.ClaimIndependentChallenge, Title: "Independent challenge", Core: true, Policy: PolicyChallenge}
	r := b.review
	if r == nil {
		c.Status = domain.ClaimInsufficient
		c.Statement = "No independent review ran."
		c.Reason = "no review artifact"
		c.Gap = "an adversarial review by a different model without the author's reasoning"
		return c
	}
	c.ArtifactIDs = []string{r.ID}
	if !b.current(*r) {
		c.Status = domain.ClaimStale
		c.Statement = "The last review looked at an earlier revision of the change."
		c.Reason = fmt.Sprintf("review observed on %s, worktree is at %s", short(r.SourceSHAs), short(b.in.CurrentSHAs))
		c.Gap = "a review of the current diff"
		return c
	}
	high := 0
	for _, f := range r.Findings {
		if f.Severity == "high" {
			high++
		}
	}
	sameModel := b.diff != nil && (b.diff.Model == r.Model || b.diff.Model == "" || r.Model == "")
	switch {
	case r.Verdict != "approve":
		c.Status = domain.ClaimContradicted
		c.Statement = fmt.Sprintf("Reviewer requested changes with %d finding(s).", len(r.Findings))
		if strings.TrimSpace(r.Counterexample) != "" {
			c.Statement += " Counterexample: " + r.Counterexample
		}
		c.Reason = "verdict=changes_requested on the current diff"
		c.Gap = "address the findings and re-review"
	case strings.TrimSpace(r.Counterexample) != "":
		c.Status = domain.ClaimContradicted
		c.Statement = "Reviewer approved but reported a concrete counterexample: " + r.Counterexample
		c.Reason = "a counterexample outranks an approval"
		c.Gap = "resolve the counterexample"
	case high > 0:
		c.Status = domain.ClaimContradicted
		c.Statement = fmt.Sprintf("Reviewer approved but reported %d high-severity finding(s).", high)
		c.Reason = "approval is inconsistent with the findings"
		c.Gap = "resolve the high-severity findings"
	case sameModel:
		c.Status = domain.ClaimInsufficient
		c.Statement = "Reviewer approved, but used the same model as the author."
		c.Reason = "independence not established"
		c.Gap = "a review by a different model"
	case len(r.Checked) == 0:
		c.Status = domain.ClaimInsufficient
		c.Statement = "Reviewer approved without stating what it verified."
		c.Reason = "\"no findings\" is not evidence unless the reviewer says what it checked"
		c.Gap = "an explicit list of verified aspects"
	default:
		c.Status = domain.ClaimSupported
		c.Statement = fmt.Sprintf("No counterexample found by an independent reviewer (%s, no access to the author's reasoning); %d checked aspect(s), %d explicitly not checked, %d non-high finding(s).",
			orUnknown(r.Model), len(r.Checked), len(r.NotChecked), len(r.Findings))
		c.Reason = "review artifact on the current diff, verdict=approve, no counterexample, no high finding. Absence of a counterexample is not proof of correctness — see gaps"
	}
	return c
}

func (b *builder) claimIntegration() domain.Claim {
	c := domain.Claim{Type: domain.ClaimIntegrationChecked, Title: "Integration checked", Core: b.in.IntegrationConfigured, Policy: PolicyIntegrate}
	if !b.in.IntegrationConfigured {
		c.Status = domain.ClaimInsufficient
		c.Statement = "No integration check is configured for this repository."
		c.Reason = "the repository policy has no integration check (start command + HTTP checks)"
		c.Gap = "not checked — behaviour behind HTTP/RPC boundaries, real datastores and logs is unverified"
		return c
	}
	if len(b.integ) == 0 {
		if len(b.integOld) > 0 {
			c.Status = domain.ClaimStale
			c.Statement = "The integration check ran on an earlier revision of the change."
			c.Reason = "no integration_run artifact on the current state"
			c.ArtifactIDs = ids(b.integOld)
		} else {
			c.Status = domain.ClaimInsufficient
			c.Statement = "The configured integration check has not run on the current code."
			c.Reason = "no integration_run artifact on the current state"
		}
		c.Gap = "run the integration check on the current code"
		return c
	}
	c.ArtifactIDs = ids(b.integ)
	for _, a := range b.integ {
		if a.Summary == "unavailable" || a.Summary == "timeout" || a.TimedOut {
			// The service could not be exercised: nothing was observed, so
			// nothing is contradicted — but nothing is supported either.
			c.Status = domain.ClaimInsufficient
			c.Statement = "The integration check could not run: " + a.Summary + "."
			c.Reason = "integration_run artifact with outcome " + a.Summary + " (service unavailable or timed out)"
			c.Gap = "a run in which the service starts and the checks execute"
			return c
		}
		if a.Passed == nil || !*a.Passed {
			c.Status = domain.ClaimContradicted
			failed := testsWith(a.Tests, "fail")
			if len(failed) > 0 {
				c.Statement = "Integration check FAILED on the current code: " + joinMax(failed, 5) + "."
			} else {
				c.Statement = "The service could not be started or probed on the current code."
			}
			c.Reason = "integration_run artifact with failing checks on the current state"
			c.Gap = "passing integration checks"
			return c
		}
	}
	names := []string{}
	for _, a := range b.integ {
		names = append(names, testsWith(a.Tests, "pass")...)
	}
	c.Status = domain.ClaimSupported
	c.Statement = fmt.Sprintf("Service started from the worktree and %d HTTP check(s) passed: %s.", len(names), joinMax(names, 5))
	c.Reason = "integration_run on the current state; every configured check passed"
	if len(b.integBase) > 0 {
		for _, a := range b.integBase {
			if a.Passed != nil && !*a.Passed {
				c.Reason += "; the same checks FAILED on the baseline (behaviour change observed end-to-end)"
				c.ArtifactIDs = append(c.ArtifactIDs, a.ID)
				break
			}
		}
	}
	c.Scope = "only the configured checks against the service started in isolation; no real datastore, no other services"
	return c
}

func (b *builder) claimCrossService() domain.Claim {
	c := domain.Claim{Type: domain.ClaimCrossServiceImpact, Title: "Cross-service impact", Core: false, Status: domain.ClaimInsufficient, Policy: PolicyCross}
	if len(b.t.Repos) <= 1 {
		c.Statement = "Only one repository was in scope; callers outside it were not examined."
		c.Reason = "no other repository was part of the task"
	} else {
		c.Statement = fmt.Sprintf("%d repositories were in scope, but no cross-repository test exists.", len(b.t.Repos))
		c.Reason = "each repository was tested in isolation"
	}
	c.Gap = "not checked — consumers of the changed behaviour in other services"
	return c
}

// ---------- gaps, risks, verdict ----------

func (b *builder) gapsAndRisks() {
	p := &b.p
	for _, c := range p.Claims {
		switch c.Status {
		case domain.ClaimContradicted, domain.ClaimBlocked:
			p.Contradictions = append(p.Contradictions, c.Title+": "+c.Statement)
		case domain.ClaimStale:
			p.Gaps = append(p.Gaps, "STALE — "+c.Title+": "+c.Statement)
		case domain.ClaimInsufficient:
			if c.Gap != "" {
				p.Gaps = append(p.Gaps, c.Title+": "+c.Gap)
			}
		}
	}
	if b.review != nil && b.current(*b.review) {
		for _, nc := range b.review.NotChecked {
			p.Gaps = append(p.Gaps, "reviewer did not check: "+nc)
		}
	}
	if b.ignored > 0 {
		p.Gaps = append(p.Gaps, fmt.Sprintf("%d artifact(s) ignored: foreign task or workspace provenance", b.ignored))
	}
	if b.in.SourceUnknown {
		p.Gaps = append(p.Gaps, "current source state could not be observed; nothing can be current")
	}
	if b.review != nil {
		for _, f := range b.review.Findings {
			p.Risks = append(p.Risks, domain.Risk{Severity: f.Severity, Source: "reviewer", Text: f.Issue, File: f.File})
		}
		if strings.TrimSpace(b.review.Counterexample) != "" {
			p.Risks = append(p.Risks, domain.Risk{Severity: "high", Source: "reviewer", Text: "counterexample: " + b.review.Counterexample})
		}
	}
	if len(p.Change.TestFiles) > 0 {
		p.Risks = append(p.Risks, domain.Risk{Severity: "medium", Source: "engine",
			Text: "the author modified test files (" + strings.Join(p.Change.TestFiles, ", ") + "); the original tests were replayed against the change — see Change verified"})
	}
	for _, d := range b.in.Decisions {
		if d.Status == "resolved" {
			p.Risks = append(p.Risks, domain.Risk{Severity: "medium", Source: "engine",
				Text: fmt.Sprintf("workflow needed a human decision: %q → %s", d.Question, d.ChosenOption)})
		}
	}
	for _, r := range researcherRisks(b.in.Evidence) {
		p.Risks = append(p.Risks, domain.Risk{Severity: "unknown", Source: "researcher (agent-reported, unverified)", Text: r})
	}
	// Severity order: high, medium, low, unknown.
	rank := map[string]int{"high": 0, "medium": 1, "low": 2, "unknown": 3}
	sort.SliceStable(p.Risks, func(i, j int) bool { return rank[p.Risks[i].Severity] < rank[p.Risks[j].Severity] })
}

func (b *builder) verdict() (domain.ClaimStatus, string) {
	t := b.t
	if t.Status == domain.StatusFailed {
		return domain.ClaimBlocked, "task failed: " + t.FailureReason
	}
	var notSupported, stale []string
	for _, c := range b.p.Claims {
		if !c.Core {
			continue
		}
		switch c.Status {
		case domain.ClaimContradicted, domain.ClaimBlocked:
			return domain.ClaimBlocked, "evidence contradicts a core claim: " + c.Title
		case domain.ClaimStale:
			stale = append(stale, c.Title)
		case domain.ClaimInsufficient:
			notSupported = append(notSupported, c.Title)
		}
	}
	if len(stale) > 0 {
		return domain.ClaimStale, "the code changed after the evidence was collected: " + strings.Join(stale, ", ")
	}
	if t.Status != domain.StatusDone {
		return domain.ClaimInsufficient, "the workflow has not finished (" + string(t.Status) + ")"
	}
	if len(notSupported) > 0 {
		return domain.ClaimInsufficient, "core claims without evidence: " + strings.Join(notSupported, ", ")
	}
	return domain.ClaimSupported, "every core claim is backed by a persisted artifact on the current source state; see gaps for what was not checked"
}

// ---------- helpers ----------

func testsWith(tests []domain.TestCase, status string) []string {
	var out []string
	for _, tc := range tests {
		if tc.Status == status {
			out = append(out, tc.Name)
		}
	}
	return out
}

func has(tests []domain.TestCase, name, status string) bool {
	for _, tc := range tests {
		if tc.Name == name && tc.Status == status {
			return true
		}
	}
	return false
}

func allPassedNow(failedBefore []string, after []domain.TestCase) bool {
	if after == nil {
		return false // unknown runner output cannot confirm the test ran
	}
	for _, n := range failedBefore {
		if !has(after, n, "pass") {
			return false
		}
	}
	return true
}

func joinMax(names []string, n int) string {
	if len(names) > n {
		return strings.Join(names[:n], ", ") + fmt.Sprintf(" +%d more", len(names)-n)
	}
	return strings.Join(names, ", ")
}

func short(m map[string]string) string {
	if len(m) == 0 {
		return "unknown"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		if len(v) > 10 {
			v = v[:10]
		}
		parts = append(parts, k+"@"+v)
	}
	return strings.Join(parts, ",")
}

func researcherRisks(evidence []domain.Evidence) []string {
	var out []string
	for _, ev := range evidence {
		if ev.Level != domain.EvidenceCodeInspected {
			continue
		}
		for _, line := range strings.Split(ev.Detail, "\n") {
			if strings.HasPrefix(line, "risk: ") {
				out = append(out, strings.TrimPrefix(line, "risk: "))
			}
		}
	}
	return out
}

// IsTestFile reports whether a repo-relative path looks like a test file.
func IsTestFile(f string) bool {
	base := strings.ToLower(filepath.Base(f))
	lf := strings.ToLower(f)
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		strings.Contains(lf, "/tests/") || strings.Contains(lf, "/__tests__/") || strings.Contains(lf, "/testdata/")
}

func ids(as []domain.Artifact) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return "model unknown"
	}
	return s
}

// fingerprint hashes the decision-relevant content (not timestamps/version).
func fingerprint(p domain.Packet) string {
	type key struct {
		Status  domain.TaskStatus
		Verdict domain.ClaimStatus
		Source  domain.SourceState
		Claims  []domain.Claim
		Gaps    []string
		Risks   []domain.Risk
		Change  domain.ChangeSummary
	}
	k := key{p.TaskStatus, p.Verdict, p.Source, p.Claims, p.Gaps, p.Risks, p.Change}
	b, _ := json.Marshal(k)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// Latest returns the most recent packet or nil.
func Latest(ps []domain.Packet) *domain.Packet {
	if len(ps) == 0 {
		return nil
	}
	return &ps[len(ps)-1]
}

// NextVersion decides whether a freshly built packet is a new version.
func NextVersion(existing []domain.Packet, fresh domain.Packet) (domain.Packet, bool) {
	if l := Latest(existing); l != nil && l.Fingerprint == fresh.Fingerprint {
		return *l, false
	}
	fresh.Version = len(existing) + 1
	fresh.BuiltAt = time.Now().UTC()
	return fresh, true
}
