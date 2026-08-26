package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"orchestrator/internal/domain"
	"orchestrator/internal/executor"
)

// P6 — crash / restart / cancel. A "crash" is simulated by an executor that
// aborts mid-step (files written, no result) or by cancelling the context
// at the step boundary, then reopening the store in a fresh engine as a
// restarted process would. After restart the system must be able to say
// what was completed, what needs a rerun, and must never mark unknown work
// complete.

func crashScenario(t *testing.T) (string, string, *executor.ScriptExecutor) {
	tmp := t.TempDir()
	repo := fixtureRepo(t, tmp)
	fixed := replaceOnce(t, readFixture(t, "store.go"), `day.Format("2006-01-02")`, `day.UTC().Format("2006-01-02")`)
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low","root_cause":{"statement":"dayKey keys by local day","file":"reservations/store.go","line":33}}`)},
		"developer":  devStep("completed", "utc", []string{"reservations/store.go", fixed}),
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[],"checked":["x"]}`)},
	}}
	return tmp, repo, sc
}

func TestCrashDuringResearchIsInterruptedNotDone(t *testing.T) {
	tmp, repo, sc := crashScenario(t)
	sc.Steps["researcher"] = executor.ScriptStep{Fail: "process died"}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTaskSpec(TaskSpec{Goal: "Fix duplicate reservation", Repos: []string{repo}, ReproCommand: "go test -run " + tzTest + " ./..."})
	_ = e.RunTask(context.Background(), task.ID)
	// An executor error pauses on a decision (retry/abort) — nothing is
	// invented. The baseline artifacts that were produced remain valid.
	got, _ := e.Store.GetTask(task.ID)
	if got.Status != domain.StatusAwaitingDecision {
		t.Fatalf("status %s", got.Status)
	}
	v, _ := e.PacketState(task.ID)
	if v.Packet.Verdict == domain.ClaimSupported || claimByType(v.Packet, domain.ClaimProblemReproduced).Status != domain.ClaimSupported {
		t.Fatalf("packet after crash: %s; reproduced=%s", v.Packet.Verdict, claimByType(v.Packet, domain.ClaimProblemReproduced).Status)
	}
	if claimByType(v.Packet, domain.ClaimChangeVerified).Status == domain.ClaimSupported {
		t.Fatal("change verified without any change")
	}
}

func TestCrashDuringImplementationLeavesNoDiffArtifact(t *testing.T) {
	tmp, repo, sc := crashScenario(t)
	// Developer writes files, then the process dies before reporting.
	dev := sc.Steps["developer"]
	dev.Fail = "killed after writing files"
	sc.Steps["developer"] = dev
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTaskSpec(TaskSpec{Goal: "Fix duplicate reservation", Repos: []string{repo}, ReproCommand: "go test -run " + tzTest + " ./..."})
	_ = e.RunTask(context.Background(), task.ID)
	got, _ := e.Store.GetTask(task.ID)
	if got.Status != domain.StatusAwaitingDecision {
		t.Fatalf("status %s", got.Status)
	}
	arts, _ := e.Store.Artifacts(task.ID)
	for _, a := range arts {
		if a.Kind == domain.ArtDiff {
			t.Fatal("a diff artifact was recorded for an unfinished implementation")
		}
	}
	// Restart: the worktree has uncommitted edits → whatever the packet says,
	// nothing on it can be current.
	e2 := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	v, _ := e2.PacketState(task.ID)
	if v.Packet.Verdict == domain.ClaimSupported {
		t.Fatal("supported after a crash mid-implementation")
	}
	if !v.Packet.Source.Dirty {
		t.Fatal("dirty worktree not reported after crash")
	}
}

func TestCrashDuringTestExecutionRecoversAsInterrupted(t *testing.T) {
	tmp, repo, sc := crashScenario(t)
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTaskSpec(TaskSpec{Goal: "Fix duplicate reservation", Repos: []string{repo}, ReproCommand: "go test -run " + tzTest + " ./..."})
	// Run understand+implement, then "crash" by cancelling at the verify step.
	ctx, cancel := context.WithCancel(context.Background())
	e.OnEvent = func(ev domain.Event) {
		if ev.Type == domain.EvPhaseChanged && ev.Data["to"] == string(domain.StatusVerifying) {
			cancel()
		}
	}
	_ = e.RunTask(ctx, task.ID)
	e2 := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	if err := e2.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	got, _ := e2.Store.GetTask(task.ID)
	if got.Status != domain.StatusInterrupted {
		t.Fatalf("status %s", got.Status)
	}
	v, _ := e2.PacketState(task.ID)
	if v.Packet.Verdict == domain.ClaimSupported || claimByType(v.Packet, domain.ClaimChangeVerified).Status == domain.ClaimSupported {
		t.Fatalf("verified without a completed test run: %s", v.Packet.VerdictWhy)
	}
	// Resume completes the work honestly: the diff artifact from before the
	// crash is still current (same SHA), tests run now.
	if _, err := e2.Resume(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := e2.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = e2.PacketState(task.ID)
	if v.Task.Status != domain.StatusDone || v.Packet.Verdict != domain.ClaimSupported {
		t.Fatalf("after resume: %s / %s (%s)", v.Task.Status, v.Packet.Verdict, v.Packet.VerdictWhy)
	}
}

func TestCrashAfterArtifactsBeforePacketRebuildsIdentically(t *testing.T) {
	tmp, repo, sc := crashScenario(t)
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTaskSpec(TaskSpec{Goal: "Fix duplicate reservation", Repos: []string{repo}, ReproCommand: "go test -run " + tzTest + " ./..."})
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	v1, _ := e.PacketState(task.ID)
	// Crash before packets.jsonl was written: delete it.
	if err := os.Remove(filepath.Join(tmp, "data", "tasks", task.ID, "packets.jsonl")); err != nil {
		t.Fatal(err)
	}
	e2 := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	v2, _ := e2.PacketState(task.ID)
	if v2.Packet.Fingerprint != v1.Packet.Fingerprint || v2.Packet.Verdict != v1.Packet.Verdict {
		t.Fatalf("packet not reproducible from artifacts: %s vs %s", v2.Packet.Fingerprint, v1.Packet.Fingerprint)
	}
}

func TestStaleLeaseFromDeadWorkerDoesNotBlockRestart(t *testing.T) {
	tmp, repo, sc := crashScenario(t)
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTaskSpec(TaskSpec{Goal: "Fix duplicate reservation", Repos: []string{repo}, ReproCommand: "go test -run " + tzTest + " ./..."})
	// A dead worker leaves .lock on disk without holding the flock.
	os.WriteFile(filepath.Join(tmp, "data", "tasks", task.ID, ".lock"), []byte("pid 4242"), 0o644)
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("stale lock file blocked execution: %v", err)
	}
	got, _ := e.Store.GetTask(task.ID)
	if got.Status != domain.StatusDone {
		t.Fatalf("%s", got.Status)
	}
}

// Re-verify after a later edit: the STALE packet is replaced by fresh
// evidence bound to the new HEAD; old versions remain.
func TestReverifyAfterEditProducesFreshEvidence(t *testing.T) {
	tmp, repo, sc := crashScenario(t)
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTaskSpec(TaskSpec{Goal: "Fix duplicate reservation", Repos: []string{repo}, ReproCommand: "go test -run " + tzTest + " ./..."})
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(e.WS.Root, task.ID, "reservations", "store.go")
	b, _ := os.ReadFile(wt)
	os.WriteFile(wt, append(b, []byte("\n// later edit\n")...), 0o644)
	v, _ := e.PacketState(task.ID)
	if v.Packet.Verdict != domain.ClaimStale {
		t.Fatalf("dirty edit → %s", v.Packet.Verdict)
	}
	if _, err := e.Reverify(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	v2, _ := e.PacketState(task.ID)
	if v2.Task.Status != domain.StatusDone || v2.Packet.Verdict != domain.ClaimSupported || v2.Packet.Version < 3 {
		t.Fatalf("after re-verify: %s %s v%d (%s)", v2.Task.Status, v2.Packet.Verdict, v2.Packet.Version, v2.Packet.VerdictWhy)
	}
	if v2.Packet.Source.HeadSHAs["reservations"] == v.Packet.Source.HeadSHAs["reservations"] {
		t.Fatal("re-verify did not commit the edit")
	}
}
