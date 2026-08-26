package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orchestrator/internal/domain"
	"orchestrator/internal/executor"
	"orchestrator/internal/github"
)

// P4: verify an EXISTING change (a PR head) — commit A is verified, then a
// new commit B is pushed: A's packet must not vouch for B, and moving the
// worktree to B makes the old evidence STALE.
func TestVerifyOnlyModeAndStaleOnNewPush(t *testing.T) {
	tmp := t.TempDir()
	repo := fixtureRepo(t, tmp)
	fixed := replaceOnce(t, readFixture(t, "store.go"), `day.Format("2006-01-02")`, `day.UTC().Format("2006-01-02")`)
	// The "PR": branch fix with commit A on top of the buggy base.
	base := strings.TrimSpace(gitOut(t, repo, "rev-parse", "--abbrev-ref", "HEAD"))
	gitRun(t, repo, "checkout", "-q", "-b", "fix")
	if err := os.WriteFile(filepath.Join(repo, "store.go"), []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "commit", "-qam", "A: normalise to UTC")
	gitRun(t, repo, "checkout", "-q", base)
	shaA := strings.TrimSpace(gitOut(t, repo, "rev-parse", "fix"))

	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","key_files":["reservations/store.go"],"uncertainty":"low","root_cause":{"statement":"dayKey keys by local day","file":"reservations/store.go","line":33}}`)},
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[],"checked":["key derivation"],"not_checked":["handler"]}`)},
		// No developer step: verify-only tasks must never call it.
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, err := e.CreateTaskSpec(TaskSpec{
		Goal: "Fix duplicate reservation across timezones", Repos: []string{repo},
		ReproCommand: "go test -run " + tzTest + " ./...", HeadRef: "fix",
		PR: &domain.PullRequestRef{Owner: "acme", Repo: "reservations", Number: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	v, err := e.PacketState(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Task.Status != domain.StatusDone || v.Packet.Verdict != domain.ClaimSupported {
		t.Fatalf("status=%s verdict=%s: %s", v.Task.Status, v.Packet.Verdict, v.Packet.VerdictWhy)
	}
	if v.Packet.Source.HeadSHAs["reservations"] != shaA || v.Packet.Source.BaseSHAs["reservations"] == shaA {
		t.Fatalf("packet not bound to head A: %+v", v.Packet.Source)
	}
	for _, r := range v.Runs {
		if r.Role == "developer" {
			t.Fatal("developer must not run in verify-only mode")
		}
	}
	if st := github.BuildStatus(v.Packet, "reservations", shaA, "u"); st.State != "success" {
		t.Fatalf("status for A: %+v", st)
	}

	// Push commit B to the PR branch.
	gitRun(t, repo, "checkout", "-q", "fix")
	if err := os.WriteFile(filepath.Join(repo, "store.go"), []byte(fixed+"\n// B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "commit", "-qam", "B: follow-up")
	gitRun(t, repo, "checkout", "-q", base)
	shaB := strings.TrimSpace(gitOut(t, repo, "rev-parse", "fix"))

	// A's packet must not vouch for B.
	if st := github.BuildStatus(v.Packet, "reservations", shaB, "u"); st.State == "success" || !strings.Contains(st.Description, "NOT VERIFIED") {
		t.Fatalf("A's packet vouched for B: %+v", st)
	}
	// Re-pointing the case's worktree at B (what a webhook refresh would do)
	// turns every piece of evidence stale — and the old version stays.
	if _, err := e.WS.ApplyHead(context.Background(), v.Task, "fix"); err != nil {
		t.Fatal(err)
	}
	v2, err := e.PacketState(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Packet.Verdict != domain.ClaimStale || v2.Packet.Version != 2 {
		t.Fatalf("after push: verdict=%s v%d (%s)", v2.Packet.Verdict, v2.Packet.Version, v2.Packet.VerdictWhy)
	}
	old, err := e.PacketVersion(task.ID, 1)
	if err != nil || old.Verdict != domain.ClaimSupported || old.Source.HeadSHAs["reservations"] != shaA {
		t.Fatalf("old packet must remain inspectable: %v %+v", err, old)
	}
	if st := github.BuildStatus(v2.Packet, "reservations", shaB, "u"); st.State == "success" {
		t.Fatalf("stale packet reported success: %+v", st)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitCmd(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}
