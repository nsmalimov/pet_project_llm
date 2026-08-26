// Package proof derives a change-assurance packet (claims → evidence →
// verdict) strictly from persisted artifacts. It never reads agent free text
// as proof: an artifact must exist for a claim to be supported, a
// contradicting artifact makes it contradicted, and a missing one makes it
// insufficient. The builder is a pure function so it can be unit-tested and
// re-run at any time; identical inputs yield an identical fingerprint.
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

// Input is everything the builder may look at. All of it is persisted state.
type Input struct {
	Task      *domain.Task
	Artifacts []domain.Artifact
	Evidence  []domain.Evidence
	Runs      []domain.AgentRun
	Decisions []domain.Decision
	// FileExists lets the builder check that a root-cause location really
	// exists in the worktree; nil disables the check (treated as unknown).
	FileExists func(repoRelPath string) bool
}

// WorktreeFileExists returns a FileExists func bound to a task's worktree.
func WorktreeFileExists(t *domain.Task) func(string) bool {
	return func(p string) bool {
		if t.State.WorktreeRoot == "" {
			return false
		}
		_, err := os.Stat(filepath.Join(t.State.WorktreeRoot, filepath.FromSlash(p)))
		return err == nil
	}
}

// Build derives a packet. Version is assigned by the caller.
func Build(in Input) domain.Packet {
	t := in.Task
	p := domain.Packet{
		TaskID:     t.ID,
		TaskStatus: t.Status,
		Gaps:       []string{},
		Risks:      []domain.Risk{},
	}

	// ---- index artifacts ----
	var baselines, tests []domain.Artifact
	var lastDiff, lastReview, lastRootCause *domain.Artifact
	for i := range in.Artifacts {
		a := &in.Artifacts[i]
		switch a.Kind {
		case domain.ArtBaselineRun:
			baselines = append(baselines, *a)
		case domain.ArtTestRun:
			tests = append(tests, *a)
		case domain.ArtDiff:
			lastDiff = a
		case domain.ArtReview:
			lastReview = a
		case domain.ArtRootCause:
			lastRootCause = a
		}
	}
	// Only the runs after the *last* diff count as "after the change".
	var after []domain.Artifact
	for _, a := range tests {
		if lastDiff == nil || !a.At.Before(lastDiff.At) {
			after = append(after, a)
		}
	}

	// ---- change summary ----
	if lastDiff != nil {
		p.Change = domain.ChangeSummary{
			Files: lastDiff.Files, Branch: lastDiff.Branch, Commits: lastDiff.Commits,
			AuthorModel: lastDiff.Model, DiffArtifact: lastDiff.ID,
		}
		for _, f := range lastDiff.Files {
			if isTestFile(f) {
				p.Change.TestFiles = append(p.Change.TestFiles, f)
			}
		}
	}
	if p.Change.Files == nil {
		p.Change.Files = []string{}
	}

	// ---- claims ----
	p.Claims = append(p.Claims, claimReproduced(t, baselines))
	p.Claims = append(p.Claims, claimRootCause(in, lastRootCause, lastDiff, baselines, after))
	p.Claims = append(p.Claims, claimChangeVerified(t, lastDiff, baselines, after, p.Change.TestFiles))
	p.Claims = append(p.Claims, claimChallenge(lastReview, lastDiff))
	p.Claims = append(p.Claims, claimIntegration(t))
	p.Claims = append(p.Claims, claimCrossService(t, lastReview))

	// ---- gaps: every non-supported claim states what is missing ----
	for _, c := range p.Claims {
		if c.Status != domain.ClaimSupported && c.Gap != "" {
			p.Gaps = append(p.Gaps, c.Title+": "+c.Gap)
		}
	}
	if lastReview != nil {
		for _, nc := range lastReview.NotChecked {
			p.Gaps = append(p.Gaps, "reviewer did not check: "+nc)
		}
	}

	// ---- risks ----
	if lastReview != nil {
		for _, f := range lastReview.Findings {
			p.Risks = append(p.Risks, domain.Risk{Severity: f.Severity, Source: "reviewer", Text: f.Issue, File: f.File})
		}
		if strings.TrimSpace(lastReview.Counterexample) != "" {
			p.Risks = append(p.Risks, domain.Risk{Severity: "high", Source: "reviewer", Text: "counterexample: " + lastReview.Counterexample})
		}
	}
	if len(p.Change.TestFiles) > 0 {
		p.Risks = append(p.Risks, domain.Risk{Severity: "medium", Source: "engine",
			Text: "the author modified test files (" + strings.Join(p.Change.TestFiles, ", ") + "); the after-change run used the author's tests, the baseline used the original ones"})
	}
	for _, d := range in.Decisions {
		if d.Status == "resolved" {
			p.Risks = append(p.Risks, domain.Risk{Severity: "medium", Source: "engine",
				Text: fmt.Sprintf("workflow needed a human decision: %q → %s", d.Question, d.ChosenOption)})
		}
	}
	for _, r := range researcherRisks(in.Runs, in.Evidence) {
		p.Risks = append(p.Risks, domain.Risk{Severity: "unknown", Source: "researcher (agent-reported, unverified)", Text: r})
	}

	// ---- verdict ----
	p.Verdict, p.VerdictWhy = verdict(t, p.Claims)

	// ---- legacy confidence ----
	best := domain.EvidenceAssumed
	for _, ev := range in.Evidence {
		if ev.Level.Rank() > best.Rank() {
			best = ev.Level
		}
	}
	p.Confidence = string(best)

	p.Fingerprint = fingerprint(p)
	return p
}

func verdict(t *domain.Task, claims []domain.Claim) (domain.ClaimStatus, string) {
	if t.Status == domain.StatusFailed {
		return domain.ClaimBlocked, "task failed: " + t.FailureReason
	}
	var notSupported []string
	for _, c := range claims {
		if !c.Core {
			continue
		}
		switch c.Status {
		case domain.ClaimContradicted, domain.ClaimBlocked:
			return domain.ClaimBlocked, "evidence contradicts a core claim: " + c.Title
		case domain.ClaimInsufficient:
			notSupported = append(notSupported, c.Title)
		}
	}
	if t.Status != domain.StatusDone {
		return domain.ClaimInsufficient, "the workflow has not finished (" + string(t.Status) + ")"
	}
	if len(notSupported) > 0 {
		return domain.ClaimInsufficient, "core claims without evidence: " + strings.Join(notSupported, ", ")
	}
	return domain.ClaimSupported, "every core claim is backed by a persisted artifact; see gaps for what was not checked"
}

// ---------- individual claims ----------

func claimReproduced(t *domain.Task, baselines []domain.Artifact) domain.Claim {
	c := domain.Claim{Type: domain.ClaimProblemReproduced, Title: "Problem reproduced", Core: t.Kind == domain.KindBugfix}
	if t.Kind != domain.KindBugfix {
		c.Status = domain.ClaimInsufficient
		c.Statement = "Not a bugfix task; no failing behaviour was expected on the baseline."
		c.Reason = "task kind is " + string(t.Kind)
		if len(baselines) > 0 {
			c.ArtifactIDs = ids(baselines)
			c.Reason += "; baseline runs recorded for comparison"
		}
		return c
	}
	if len(baselines) == 0 {
		c.Status = domain.ClaimInsufficient
		c.Statement = "No baseline run exists."
		c.Reason = "the reproduction step did not run (no test/repro command detected or the task was created before baseline support)"
		c.Gap = "run the failing test on the unchanged code"
		return c
	}
	c.ArtifactIDs = ids(baselines)
	// Prefer the narrow repro command when present.
	var failed, passed []domain.Artifact
	for _, b := range baselines {
		if b.Passed != nil && !*b.Passed {
			failed = append(failed, b)
		} else {
			passed = append(passed, b)
		}
	}
	if len(failed) > 0 {
		c.Status = domain.ClaimSupported
		c.Statement = fmt.Sprintf("`%s` fails on the unchanged code (exit %d).", failed[0].Command, failed[0].ExitCode)
		c.Reason = "baseline run recorded before any implementation; " + failingTestNames(failed[0].Output)
		return c
	}
	c.Status = domain.ClaimContradicted
	c.Statement = fmt.Sprintf("`%s` PASSES on the unchanged code.", passed[0].Command)
	c.Reason = "the test command does not exercise the reported bug, so nothing that passes later proves the bug is fixed"
	c.Gap = "provide a repro command / test that fails before the change"
	return c
}

func claimRootCause(in Input, rc, diff *domain.Artifact, baselines, after []domain.Artifact) domain.Claim {
	c := domain.Claim{Type: domain.ClaimRootCauseSupported, Title: "Root cause supported", Core: in.Task.Kind == domain.KindBugfix}
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
	} else if in.FileExists != nil && !in.FileExists(rc.RootCause.File) {
		problems = append(problems, "named file does not exist in the worktree")
	}
	touched := false
	if diff != nil {
		c.ArtifactIDs = append(c.ArtifactIDs, diff.ID)
		for _, f := range diff.Files {
			if f == rc.RootCause.File {
				touched = true
			}
		}
	}
	if rc.RootCause.File != "" && !touched {
		problems = append(problems, "the fix does not modify the named file")
	}
	flip := baselineFailedThenPassed(baselines, after)
	if !flip {
		problems = append(problems, "no baseline-fail → after-pass pair confirms the location")
	} else {
		c.ArtifactIDs = append(c.ArtifactIDs, ids(baselines)...)
		c.ArtifactIDs = append(c.ArtifactIDs, ids(after)...)
	}
	if len(problems) == 0 {
		c.Status = domain.ClaimSupported
		c.Reason = "hypothesis names an existing file, the fix modifies exactly that file, and the failing test flipped to passing"
		return c
	}
	c.Status = domain.ClaimInsufficient
	c.Reason = "hypothesis is agent-reported and not cross-checked: " + strings.Join(problems, "; ")
	c.Gap = strings.Join(problems, "; ")
	return c
}

func claimChangeVerified(t *domain.Task, diff *domain.Artifact, baselines, after []domain.Artifact, testFiles []string) domain.Claim {
	c := domain.Claim{Type: domain.ClaimChangeVerified, Title: "Change verified", Core: true}
	if diff == nil || len(diff.Files) == 0 {
		c.Status = domain.ClaimInsufficient
		c.Statement = "No change exists."
		c.Reason = "no diff artifact was recorded"
		c.Gap = "an implementation"
		return c
	}
	c.ArtifactIDs = append(c.ArtifactIDs, diff.ID)
	if len(after) == 0 {
		c.Status = domain.ClaimInsufficient
		c.Statement = "No tests ran after the change."
		c.Reason = "no test_run artifact after the last diff"
		c.Gap = "run the test suite on the changed code"
		return c
	}
	c.ArtifactIDs = append(c.ArtifactIDs, ids(after)...)
	var failing []domain.Artifact
	for _, a := range after {
		if a.Passed == nil || !*a.Passed {
			failing = append(failing, a)
		}
	}
	if len(failing) > 0 {
		c.Status = domain.ClaimContradicted
		c.Statement = fmt.Sprintf("`%s` FAILS after the change (exit %d).", failing[0].Command, failing[0].ExitCode)
		c.Reason = "the latest test run after the last diff failed"
		c.Gap = "a passing run"
		return c
	}
	cmds := make([]string, 0, len(after))
	for _, a := range after {
		cmds = append(cmds, "`"+a.Command+"`")
	}
	if t.Kind == domain.KindBugfix {
		if baselineFailedThenPassed(baselines, after) {
			c.ArtifactIDs = append(c.ArtifactIDs, ids(baselines)...)
			c.Status = domain.ClaimSupported
			c.Statement = "The command that failed on the baseline passes after the change; " + strings.Join(cmds, ", ") + " pass."
			c.Reason = "same command: baseline exit ≠ 0 → after-change exit 0"
			return c
		}
		c.Status = domain.ClaimInsufficient
		c.Statement = strings.Join(cmds, ", ") + " pass after the change."
		c.Reason = "tests pass, but no baseline failure proves they exercise the bug"
		c.Gap = "a baseline failure of the same command"
		return c
	}
	c.Status = domain.ClaimSupported
	c.Statement = strings.Join(cmds, ", ") + " pass after the change."
	c.Reason = "real test run recorded after the last diff"
	return c
}

func claimChallenge(review, diff *domain.Artifact) domain.Claim {
	c := domain.Claim{Type: domain.ClaimIndependentChallenge, Title: "Independent challenge", Core: true}
	if review == nil {
		c.Status = domain.ClaimInsufficient
		c.Statement = "No independent review ran."
		c.Reason = "no review artifact"
		c.Gap = "an adversarial review by a different model without the author's reasoning"
		return c
	}
	if diff != nil && review.At.Before(diff.At) {
		c.Status = domain.ClaimInsufficient
		c.Statement = "The last review predates the last change."
		c.Reason = "the diff was modified after the review"
		c.Gap = "a review of the current diff"
		c.ArtifactIDs = []string{review.ID, diff.ID}
		return c
	}
	c.ArtifactIDs = []string{review.ID}
	high := 0
	for _, f := range review.Findings {
		if f.Severity == "high" {
			high++
		}
	}
	sameModel := diff != nil && diff.Model != "" && diff.Model == review.Model
	switch {
	case review.Verdict == "approve" && high == 0 && strings.TrimSpace(review.Counterexample) == "" && !sameModel:
		c.Status = domain.ClaimSupported
		c.Statement = fmt.Sprintf("Reviewer (%s, no access to the author's reasoning) approved; %d finding(s), none high; %d aspect(s) checked, %d explicitly not checked.",
			orUnknown(review.Model), len(review.Findings), len(review.Checked), len(review.NotChecked))
		c.Reason = "review artifact with verdict=approve on the current diff"
	case review.Verdict == "approve" && sameModel:
		c.Status = domain.ClaimInsufficient
		c.Statement = "Reviewer approved, but used the same model as the author."
		c.Reason = "independence not established"
		c.Gap = "a review by a different model"
	case review.Verdict == "approve":
		c.Status = domain.ClaimInsufficient
		c.Statement = fmt.Sprintf("Reviewer approved but reported %d high-severity finding(s) or a counterexample.", high)
		c.Reason = "approval is inconsistent with the findings; treat as unresolved"
		c.Gap = "resolve the high-severity findings"
	default:
		c.Status = domain.ClaimContradicted
		c.Statement = fmt.Sprintf("Reviewer requested changes with %d finding(s).", len(review.Findings))
		c.Reason = "verdict=changes_requested on the current diff"
		c.Gap = "address the findings and re-review"
	}
	return c
}

func claimIntegration(t *domain.Task) domain.Claim {
	// No integration runner exists in this slice. Say so, loudly.
	return domain.Claim{
		Type: domain.ClaimIntegrationChecked, Title: "Integration checked", Core: false,
		Status:    domain.ClaimInsufficient,
		Statement: "No integration, log or trace check was executed.",
		Reason:    "this deployment has no integration check runner configured",
		Gap:       "not checked — behaviour behind HTTP/RPC boundaries, real datastores and logs is unverified",
	}
}

func claimCrossService(t *domain.Task, review *domain.Artifact) domain.Claim {
	c := domain.Claim{Type: domain.ClaimCrossServiceImpact, Title: "Cross-service impact", Core: false}
	c.Status = domain.ClaimInsufficient
	if len(t.Repos) <= 1 {
		c.Statement = "Only one repository was in scope; callers outside it were not examined."
		c.Reason = "no other repository was part of the task"
	} else {
		c.Statement = fmt.Sprintf("%d repositories were in scope, but no cross-repository test exists.", len(t.Repos))
		c.Reason = "each repository was tested in isolation"
	}
	c.Gap = "not checked — consumers of the changed behaviour in other services"
	return c
}

// ---------- helpers ----------

func baselineFailedThenPassed(baselines, after []domain.Artifact) bool {
	for _, b := range baselines {
		if b.Passed == nil || *b.Passed {
			continue
		}
		for _, a := range after {
			if a.Command == b.Command && a.Repo == b.Repo && a.Passed != nil && *a.Passed {
				return true
			}
		}
	}
	return false
}

func failingTestNames(output string) string {
	var names []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "--- FAIL: ") {
			names = append(names, strings.Fields(strings.TrimPrefix(line, "--- FAIL: "))[0])
		} else if strings.HasPrefix(line, "FAILED ") && strings.Contains(line, "::") { // pytest
			names = append(names, strings.Fields(line)[1])
		}
	}
	if len(names) == 0 {
		return "failing test names not parsed from output"
	}
	if len(names) > 5 {
		names = append(names[:5], fmt.Sprintf("+%d more", len(names)-5))
	}
	return "failing: " + strings.Join(names, ", ")
}

func researcherRisks(runs []domain.AgentRun, evidence []domain.Evidence) []string {
	// Risks the researcher reported are stored on the code_inspected evidence
	// detail as "risks: ..." lines by the engine.
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

func isTestFile(f string) bool {
	base := strings.ToLower(filepath.Base(f))
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		strings.Contains(strings.ToLower(f), "/tests/") || strings.Contains(strings.ToLower(f), "/__tests__/")
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
		Claims  []domain.Claim
		Gaps    []string
		Risks   []domain.Risk
		Change  domain.ChangeSummary
	}
	k := key{p.TaskStatus, p.Verdict, p.Claims, p.Gaps, p.Risks, p.Change}
	sort.Strings(k.Gaps)
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
