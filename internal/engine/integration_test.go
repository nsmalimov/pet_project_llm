package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"orchestrator/internal/domain"
	"orchestrator/internal/executor"
	"orchestrator/internal/repos"
)

func integrationPolicy(port int) *repos.Policy {
	return &repos.Policy{
		TestCommand: "go test ./...", ReproCommand: "go test -run " + tzTest + " ./...",
		Integration: &repos.IntegrationCheck{Start: "go run ./cmd/server", Port: port, StartupSeconds: 120, Checks: []repos.HTTPCheck{
			{Name: "health", Method: "GET", Path: "/healthz", ExpectStatus: 200, ExpectBody: "ok"},
			{Name: "first booking accepted", Method: "POST", Path: "/reserve", Body: `{"room":"101","day":"2026-03-14T23:30:00-04:00"}`, ExpectStatus: 200},
			{Name: "same UTC day from another zone rejected", Method: "POST", Path: "/reserve", Body: `{"room":"101","day":"2026-03-15T10:00:00Z"}`, ExpectStatus: 409},
		}},
	}
}

// The one real integration provider: the service is started from the
// worktree and probed. On the buggy baseline the conflict check fails; on
// the fixed code it passes → Integration checked becomes SUPPORTED with the
// baseline failure noted. A verify-only case of the unfixed code → CONTRADICTED.
func TestIntegrationCheckEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	repo := fixtureRepo(t, tmp)
	fixed := replaceOnce(t, readFixture(t, "store.go"), `day.Format("2006-01-02")`, `day.UTC().Format("2006-01-02")`)
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low","root_cause":{"statement":"dayKey keys by local day","file":"reservations/store.go","line":33}}`)},
		"developer":  devStep("completed", "utc", []string{"reservations/store.go", fixed}),
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[],"checked":["x"]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	e.Repos = repos.Open(filepath.Join(tmp, "data"), e.Policy)
	rp, err := e.Repos.Add(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Repos.SetPolicy(rp.ID, integrationPolicy(18500+int(domain.NewIDNumber()%1000))); err != nil {
		t.Fatal(err)
	}
	// Commands come from the policy: the request carries none.
	task, err := e.CreateTaskSpec(TaskSpec{Goal: "Fix duplicate reservation across timezones", Repos: []string{rp.ID}, Kind: domain.KindBugfix})
	if err != nil {
		t.Fatal(err)
	}
	if task.ReproCommand == "" || task.TestCommand != "go test ./..." {
		t.Fatalf("policy not applied: %+v", task)
	}
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	v, _ := e.PacketState(task.ID)
	c := claimByType(v.Packet, domain.ClaimIntegrationChecked)
	if v.Task.Status != domain.StatusDone || c.Status != domain.ClaimSupported || !c.Core || !strings.Contains(c.Reason, "FAILED on the baseline") {
		t.Fatalf("status=%s integration=%s core=%v reason=%s", v.Task.Status, c.Status, c.Core, c.Reason)
	}
	if v.Packet.Verdict != domain.ClaimSupported {
		t.Fatalf("verdict %s: %s", v.Packet.Verdict, v.Packet.VerdictWhy)
	}
	n := 0
	for _, a := range v.Artifacts {
		if a.Kind == domain.ArtIntegrationRun {
			n++
			if a.SourceSHAs == nil || !a.TestsParsed || len(a.Tests) != 3 {
				t.Fatalf("integration artifact incomplete: %+v", a)
			}
		}
	}
	if n != 2 {
		t.Fatalf("expected baseline + after integration runs, got %d", n)
	}
	// Verify-only case on the unfixed code (head = base): the check fails.
	gitRun(t, repo, "branch", "-q", "same")
	task2, err := e.CreateTaskSpec(TaskSpec{Goal: "Verify nothing", Repos: []string{rp.ID}, Kind: domain.KindChange, HeadRef: "same"})
	if err != nil {
		t.Fatal(err)
	}
	e.Cfg.MaxFixAttempts = 0
	_ = e.RunTask(context.Background(), task2.ID)
	v2, _ := e.PacketState(task2.ID)
	c2 := claimByType(v2.Packet, domain.ClaimIntegrationChecked)
	if c2.Status != domain.ClaimContradicted || v2.Packet.Verdict != domain.ClaimBlocked {
		t.Fatalf("unfixed head: integration=%s verdict=%s (%s)", c2.Status, v2.Packet.Verdict, v2.Packet.VerdictWhy)
	}
}
