package proof

import (
	"testing"
	"time"

	"orchestrator/internal/domain"
)

func bp(b bool) *bool { return &b }

var (
	baseSHA = map[string]string{"r": "base000"}
	headSHA = map[string]string{"r": "head111"}
)

func base() (*domain.Task, []domain.Artifact) {
	t0 := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	t := &domain.Task{ID: "task_x", Kind: domain.KindBugfix, Status: domain.StatusDone, Repos: []domain.RepoRef{{Name: "r"}},
		State: domain.TaskState{BaseSHAs: baseSHA, WorktreeRoot: "/wt"}}
	arts := []domain.Artifact{
		{ID: "a1", TaskID: "task_x", Kind: domain.ArtBaselineRun, Repo: "r", Command: "go test ./...", Passed: bp(false), ExitCode: 1,
			Tests: []domain.TestCase{{Name: "TestX", Status: "fail"}, {Name: "TestY", Status: "pass"}}, SourceSHAs: baseSHA, At: t0},
		{ID: "a2", TaskID: "task_x", Kind: domain.ArtRootCause, RootCause: &domain.RootCause{Statement: "key ignores zone", File: "r/store.go", Line: 3}, At: t0.Add(time.Minute)},
		{ID: "a3", TaskID: "task_x", Kind: domain.ArtDiff, Files: []string{"r/store.go"}, Diff: "x", Model: "sonnet", Commits: map[string]string{"r": "head111"}, SourceSHAs: headSHA, At: t0.Add(2 * time.Minute)},
		{ID: "a4", TaskID: "task_x", Kind: domain.ArtTestRun, Repo: "r", Command: "go test ./...", Passed: bp(true),
			Tests: []domain.TestCase{{Name: "TestX", Status: "pass"}, {Name: "TestY", Status: "pass"}}, SourceSHAs: headSHA, At: t0.Add(3 * time.Minute)},
		{ID: "a5", TaskID: "task_x", Kind: domain.ArtReview, Verdict: "approve", Model: "opus", Checked: []string{"key derivation"}, NotChecked: []string{"handler"}, SourceSHAs: headSHA, At: t0.Add(4 * time.Minute)},
	}
	return t, arts
}

func build(t *domain.Task, arts []domain.Artifact) domain.Packet {
	return Build(Input{Task: t, Artifacts: arts, CurrentSHAs: headSHA, FileExists: func(string) bool { return true }})
}

func status(p domain.Packet, typ domain.ClaimType) domain.ClaimStatus {
	for _, c := range p.Claims {
		if c.Type == typ {
			return c.Status
		}
	}
	return ""
}

func TestAllCoreSupported(t *testing.T) {
	task, arts := base()
	p := build(task, arts)
	if p.Verdict != domain.ClaimSupported {
		t.Fatalf("verdict %s: %s", p.Verdict, p.VerdictWhy)
	}
	for _, typ := range []domain.ClaimType{domain.ClaimProblemReproduced, domain.ClaimRootCauseSupported, domain.ClaimChangeVerified, domain.ClaimIndependentChallenge} {
		if s := status(p, typ); s != domain.ClaimSupported {
			t.Fatalf("%s = %s", typ, s)
		}
	}
	if status(p, domain.ClaimIntegrationChecked) != domain.ClaimInsufficient {
		t.Fatal("integration must stay insufficient without a runner")
	}
	if len(p.Gaps) != 3 {
		t.Fatalf("gaps=%v", p.Gaps)
	}
	if p.Fingerprint != build(task, arts).Fingerprint {
		t.Fatal("fingerprint not deterministic")
	}
}

func TestNoArtifactsMeansInsufficient(t *testing.T) {
	task, _ := base()
	p := Build(Input{Task: task, CurrentSHAs: headSHA})
	if p.Verdict != domain.ClaimInsufficient {
		t.Fatalf("verdict %s", p.Verdict)
	}
	for _, c := range p.Claims {
		if c.Status == domain.ClaimSupported {
			t.Fatalf("claim %s supported without artifacts", c.Type)
		}
	}
}

func TestFailingAfterRunContradicts(t *testing.T) {
	task, arts := base()
	arts[3].Passed = bp(false)
	p := build(task, arts)
	if status(p, domain.ClaimChangeVerified) != domain.ClaimContradicted || p.Verdict != domain.ClaimBlocked {
		t.Fatalf("%s / %s", status(p, domain.ClaimChangeVerified), p.Verdict)
	}
}

func TestRootCauseNeedsFixInNamedFile(t *testing.T) {
	task, arts := base()
	arts[1].RootCause.File = "r/other.go"
	p := build(task, arts)
	if status(p, domain.ClaimRootCauseSupported) != domain.ClaimInsufficient {
		t.Fatal("hypothesis on an untouched file must not be supported")
	}
}

func TestReviewMustPostdateDiffAndDifferInModel(t *testing.T) {
	task, arts := base()
	arts[4].Model = "sonnet"
	p := build(task, arts)
	if status(p, domain.ClaimIndependentChallenge) != domain.ClaimInsufficient {
		t.Fatal("same-model review is not independent")
	}
	arts[4].Model = "opus"
	arts[4].SourceSHAs = map[string]string{"r": "older"}
	p = build(task, arts)
	if status(p, domain.ClaimIndependentChallenge) != domain.ClaimStale {
		t.Fatal("stale review must not count")
	}
	arts[4].SourceSHAs = headSHA
	arts[4].Verdict = "changes_requested"
	p = build(task, arts)
	if status(p, domain.ClaimIndependentChallenge) != domain.ClaimContradicted || p.Verdict != domain.ClaimBlocked {
		t.Fatal("changes_requested must contradict and block")
	}
}

func TestTestsAfterOldDiffDoNotCount(t *testing.T) {
	task, arts := base()
	// A second diff after the test run: the run no longer verifies the change.
	newHead := map[string]string{"r": "head222"}
	arts = append(arts, domain.Artifact{ID: "a6", TaskID: "task_x", Kind: domain.ArtDiff, Files: []string{"r/store.go"}, Model: "sonnet", SourceSHAs: newHead, At: arts[4].At.Add(time.Minute)})
	p := Build(Input{Task: task, Artifacts: arts, CurrentSHAs: newHead, FileExists: func(string) bool { return true }})
	if status(p, domain.ClaimChangeVerified) != domain.ClaimStale {
		t.Fatalf("tests on an older revision must be stale, got %s", status(p, domain.ClaimChangeVerified))
	}
	if status(p, domain.ClaimIndependentChallenge) != domain.ClaimStale {
		t.Fatal("review of an older revision must be stale")
	}
	if p.Verdict != domain.ClaimStale {
		t.Fatalf("verdict %s", p.Verdict)
	}
}
