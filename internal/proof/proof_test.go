package proof

import (
	"testing"
	"time"

	"orchestrator/internal/domain"
)

func bp(b bool) *bool { return &b }

func base() (*domain.Task, []domain.Artifact) {
	t0 := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	t := &domain.Task{ID: "task_x", Kind: domain.KindBugfix, Status: domain.StatusDone, Repos: []domain.RepoRef{{Name: "r"}}}
	arts := []domain.Artifact{
		{ID: "a1", Kind: domain.ArtBaselineRun, Repo: "r", Command: "go test ./...", Passed: bp(false), ExitCode: 1, Output: "--- FAIL: TestX\nFAIL", At: t0},
		{ID: "a2", Kind: domain.ArtRootCause, RootCause: &domain.RootCause{Statement: "key ignores zone", File: "r/store.go", Line: 3}, At: t0.Add(time.Minute)},
		{ID: "a3", Kind: domain.ArtDiff, Files: []string{"r/store.go"}, Diff: "x", Model: "sonnet", Commits: map[string]string{"r": "abc"}, At: t0.Add(2 * time.Minute)},
		{ID: "a4", Kind: domain.ArtTestRun, Repo: "r", Command: "go test ./...", Passed: bp(true), At: t0.Add(3 * time.Minute)},
		{ID: "a5", Kind: domain.ArtReview, Verdict: "approve", Model: "opus", NotChecked: []string{"handler"}, At: t0.Add(4 * time.Minute)},
	}
	return t, arts
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
	p := Build(Input{Task: task, Artifacts: arts, FileExists: func(string) bool { return true }})
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
	if p.Fingerprint != Build(Input{Task: task, Artifacts: arts, FileExists: func(string) bool { return true }}).Fingerprint {
		t.Fatal("fingerprint not deterministic")
	}
}

func TestNoArtifactsMeansInsufficient(t *testing.T) {
	task, _ := base()
	p := Build(Input{Task: task})
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
	p := Build(Input{Task: task, Artifacts: arts, FileExists: func(string) bool { return true }})
	if status(p, domain.ClaimChangeVerified) != domain.ClaimContradicted || p.Verdict != domain.ClaimBlocked {
		t.Fatalf("%s / %s", status(p, domain.ClaimChangeVerified), p.Verdict)
	}
}

func TestRootCauseNeedsFixInNamedFile(t *testing.T) {
	task, arts := base()
	arts[1].RootCause.File = "r/other.go"
	p := Build(Input{Task: task, Artifacts: arts, FileExists: func(string) bool { return true }})
	if status(p, domain.ClaimRootCauseSupported) != domain.ClaimInsufficient {
		t.Fatal("hypothesis on an untouched file must not be supported")
	}
}

func TestReviewMustPostdateDiffAndDifferInModel(t *testing.T) {
	task, arts := base()
	arts[4].Model = "sonnet"
	p := Build(Input{Task: task, Artifacts: arts, FileExists: func(string) bool { return true }})
	if status(p, domain.ClaimIndependentChallenge) != domain.ClaimInsufficient {
		t.Fatal("same-model review is not independent")
	}
	arts[4].Model = "opus"
	arts[4].At = arts[2].At.Add(-time.Second)
	p = Build(Input{Task: task, Artifacts: arts, FileExists: func(string) bool { return true }})
	if status(p, domain.ClaimIndependentChallenge) != domain.ClaimInsufficient {
		t.Fatal("stale review must not count")
	}
	arts[4].At = arts[2].At.Add(time.Second)
	arts[4].Verdict = "changes_requested"
	p = Build(Input{Task: task, Artifacts: arts, FileExists: func(string) bool { return true }})
	if status(p, domain.ClaimIndependentChallenge) != domain.ClaimContradicted || p.Verdict != domain.ClaimBlocked {
		t.Fatal("changes_requested must contradict and block")
	}
}

func TestTestsAfterOldDiffDoNotCount(t *testing.T) {
	task, arts := base()
	// A second diff after the test run: the run no longer verifies the change.
	arts = append(arts, domain.Artifact{ID: "a6", Kind: domain.ArtDiff, Files: []string{"r/store.go"}, Model: "sonnet", At: arts[4].At.Add(time.Minute)})
	p := Build(Input{Task: task, Artifacts: arts, FileExists: func(string) bool { return true }})
	if status(p, domain.ClaimChangeVerified) != domain.ClaimInsufficient {
		t.Fatal("tests that predate the last diff must not verify it")
	}
	if status(p, domain.ClaimIndependentChallenge) != domain.ClaimInsufficient {
		t.Fatal("review that predates the last diff must not count")
	}
}
