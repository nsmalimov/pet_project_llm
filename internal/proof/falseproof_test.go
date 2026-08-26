package proof

import (
	"testing"

	"orchestrator/internal/domain"
)

// Regression tests for FALSE_PROOF_REPORT.md: every scenario below once
// produced (or could have produced) a false SUPPORTED.

func TestExitZeroWithoutTestsIsNotVerification(t *testing.T) {
	task, arts := base()
	arts[3].Tests = []domain.TestCase{} // go test -v printed no test lines
	p := build(task, arts)
	if s := status(p, domain.ClaimChangeVerified); s != domain.ClaimInsufficient {
		t.Fatalf("exit 0 with zero tests → %s, want insufficient", s)
	}
}

func TestReproTestMustRunAndPassAfterChange(t *testing.T) {
	task, arts := base()
	arts[3].Tests = []domain.TestCase{{Name: "TestX", Status: "skip"}, {Name: "TestY", Status: "pass"}}
	p := build(task, arts)
	if s := status(p, domain.ClaimChangeVerified); s != domain.ClaimInsufficient {
		t.Fatalf("skipped repro test → %s, want insufficient", s)
	}
	arts[3].Tests = []domain.TestCase{{Name: "TestY", Status: "pass"}} // TestX deleted/filtered
	p = build(task, arts)
	if s := status(p, domain.ClaimChangeVerified); s != domain.ClaimInsufficient {
		t.Fatalf("missing repro test → %s, want insufficient", s)
	}
	if status(p, domain.ClaimRootCauseSupported) == domain.ClaimSupported {
		t.Fatal("root cause must not be supported without the repro flip")
	}
}

func TestBaselineBuildErrorIsNotReproduction(t *testing.T) {
	task, arts := base()
	arts[0].Tests = []domain.TestCase{} // exit 1 but no test failed (compile error)
	p := build(task, arts)
	if s := status(p, domain.ClaimProblemReproduced); s != domain.ClaimInsufficient {
		t.Fatalf("build failure baseline → %s, want insufficient", s)
	}
	if p.Verdict == domain.ClaimSupported {
		t.Fatal("verdict must not be supported")
	}
}

func TestFlakyRunsContradict(t *testing.T) {
	task, arts := base()
	arts = append(arts, domain.Artifact{ID: "a6", TaskID: "task_x", Kind: domain.ArtTestRun, Repo: "r", Command: "go test ./...", Passed: bp(false), ExitCode: 1,
		Tests: []domain.TestCase{{Name: "TestX", Status: "fail"}}, SourceSHAs: headSHA, Repeat: 2})
	p := build(task, arts)
	if s := status(p, domain.ClaimChangeVerified); s != domain.ClaimContradicted {
		t.Fatalf("flaky → %s", s)
	}
	if p.Verdict != domain.ClaimBlocked || len(p.Contradictions) == 0 {
		t.Fatalf("verdict %s contradictions %v", p.Verdict, p.Contradictions)
	}
}

func TestTimeoutNeverLooksLikeSuccess(t *testing.T) {
	task, arts := base()
	arts[3].TimedOut = true
	arts[3].Passed = bp(false)
	p := build(task, arts)
	if s := status(p, domain.ClaimChangeVerified); s != domain.ClaimContradicted {
		t.Fatalf("after-change timeout → %s", s)
	}
	task, arts = base()
	arts[0].TimedOut = true
	p = build(task, arts)
	if s := status(p, domain.ClaimProblemReproduced); s != domain.ClaimInsufficient {
		t.Fatalf("baseline timeout → %s", s)
	}
}

func TestDirtyWorktreeIsStale(t *testing.T) {
	task, arts := base()
	p := Build(Input{Task: task, Artifacts: arts, CurrentSHAs: headSHA, CurrentDirty: true, FileExists: func(string) bool { return true }})
	if p.Verdict != domain.ClaimStale {
		t.Fatalf("dirty worktree → %s", p.Verdict)
	}
}

func TestUnknownSourceStateCannotBeCurrent(t *testing.T) {
	task, arts := base()
	p := Build(Input{Task: task, Artifacts: arts, SourceUnknown: true, FileExists: func(string) bool { return true }})
	if p.Verdict == domain.ClaimSupported {
		t.Fatal("unknown source state must not be supported")
	}
}

func TestForeignArtifactsIgnored(t *testing.T) {
	task, arts := base()
	for i := range arts {
		if arts[i].Kind == domain.ArtTestRun {
			arts[i].WorktreeRoot = "/somewhere/else"
		}
	}
	p := build(task, arts)
	if s := status(p, domain.ClaimChangeVerified); s == domain.ClaimSupported {
		t.Fatal("artifact from another workspace must not support a claim")
	}
	task, arts = base()
	arts[3].TaskID = "task_other"
	if p := build(task, arts); status(p, domain.ClaimChangeVerified) == domain.ClaimSupported {
		t.Fatal("artifact of another task must not support a claim")
	}
}

func TestAuthorModifiedTestsNeedOriginalReplay(t *testing.T) {
	task, arts := base()
	arts[2].Files = []string{"r/store.go", "r/store_test.go"}
	p := build(task, arts)
	if s := status(p, domain.ClaimChangeVerified); s != domain.ClaimInsufficient {
		t.Fatalf("modified tests without replay → %s", s)
	}
	arts = append(arts, domain.Artifact{ID: "a7", TaskID: "task_x", Kind: domain.ArtOriginalTestsRun, Repo: "r", Command: "go test ./...", Passed: bp(false), ExitCode: 1,
		Tests: []domain.TestCase{{Name: "TestX", Status: "fail"}}, SourceSHAs: headSHA})
	p = build(task, arts)
	if s := status(p, domain.ClaimChangeVerified); s != domain.ClaimContradicted {
		t.Fatalf("original tests failing → %s", s)
	}
	arts[len(arts)-1].Passed = bp(true)
	arts[len(arts)-1].Tests = []domain.TestCase{{Name: "TestX", Status: "pass"}}
	p = build(task, arts)
	if s := status(p, domain.ClaimChangeVerified); s != domain.ClaimSupported {
		t.Fatalf("original tests passing → %s (%s)", s, p.VerdictWhy)
	}
}

func TestReviewerPolicies(t *testing.T) {
	task, arts := base()
	arts[4].Counterexample = "two goroutines reserve concurrently"
	p := build(task, arts)
	if s := status(p, domain.ClaimIndependentChallenge); s != domain.ClaimContradicted {
		t.Fatalf("approve + counterexample → %s", s)
	}
	if p.Risks[0].Severity != "high" {
		t.Fatal("counterexample must be the top risk")
	}
	task, arts = base()
	arts[4].Checked = nil
	if p := build(task, arts); status(p, domain.ClaimIndependentChallenge) != domain.ClaimInsufficient {
		t.Fatal("no findings without stated coverage is not evidence")
	}
	task, arts = base()
	arts[4].Findings = []domain.Finding{{Severity: "high", Issue: "x"}}
	if p := build(task, arts); status(p, domain.ClaimIndependentChallenge) != domain.ClaimContradicted {
		t.Fatal("approve + high finding must contradict")
	}
}

func TestReproMustBeTheNarrowCommandWhenPresent(t *testing.T) {
	task, arts := base()
	task.ReproCommand = "go test -run TestX ./..."
	// Only the full suite failed on baseline (some unrelated test); the
	// narrow repro command passed.
	arts = append(arts, domain.Artifact{ID: "a8", TaskID: "task_x", Kind: domain.ArtBaselineRun, Repo: "r", Command: task.ReproCommand, Narrow: true, Passed: bp(true), Tests: []domain.TestCase{{Name: "TestX", Status: "pass"}}, SourceSHAs: baseSHA})
	p := build(task, arts)
	if s := status(p, domain.ClaimProblemReproduced); s != domain.ClaimContradicted {
		t.Fatalf("narrow repro passing on baseline → %s", s)
	}
	if status(p, domain.ClaimChangeVerified) == domain.ClaimSupported {
		t.Fatal("an unrelated baseline failure must not verify the change")
	}
}

func TestBaselineOnWrongRevisionIsStale(t *testing.T) {
	task, arts := base()
	arts[0].SourceSHAs = map[string]string{"r": "notbase"}
	p := build(task, arts)
	if s := status(p, domain.ClaimProblemReproduced); s != domain.ClaimStale {
		t.Fatalf("baseline not on base → %s", s)
	}
}
