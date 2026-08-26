package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orchestrator/internal/domain"
	"orchestrator/internal/executor"
	"orchestrator/internal/github"
	"orchestrator/internal/github/fakegh"
	"orchestrator/internal/repos"
)

// P4 — GitHub semantics against a local fake GitHub:
//
//	PR head A → verified SUPPORTED → status success for A
//	push B (3 duplicate deliveries) → exactly one new case; A's packet immutable
//	B not verified yet → status for B says NOT VERIFIED
//	revocation → RefreshPR blocks the open case visibly
//	effects ledger: a crash between POST and "done" makes the retry refuse.
func TestGitHubPRLifecycle(t *testing.T) {
	tmp := t.TempDir()
	// "origin": a bare repo with the buggy base and a feature branch with the fix.
	work := fixtureRepo(t, tmp)
	fixed := replaceOnce(t, readFixture(t, "store.go"), `day.Format("2006-01-02")`, `day.UTC().Format("2006-01-02")`)
	baseSHA := strings.TrimSpace(gitOut(t, work, "rev-parse", "HEAD"))
	gitRun(t, work, "checkout", "-q", "-b", "feature")
	os.WriteFile(filepath.Join(work, "store.go"), []byte(fixed), 0o644)
	gitRun(t, work, "commit", "-qam", "A: utc")
	shaA := strings.TrimSpace(gitOut(t, work, "rev-parse", "HEAD"))
	bare := filepath.Join(tmp, "origin.git")
	gitRun(t, tmp, "clone", "-q", "--bare", work, bare)
	// Local mirror registered in Proofline (base branch only; heads are fetched).
	mirror := filepath.Join(tmp, "mirror")
	gitRun(t, tmp, "clone", "-q", "--branch", "master", bare, mirror)
	if _, err := os.Stat(filepath.Join(mirror, ".git")); err != nil {
		gitRun(t, tmp, "clone", "-q", "--branch", "main", bare, mirror)
	}

	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low","root_cause":{"statement":"dayKey keys by local day","file":"reservations/store.go","line":33}}`)},
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[],"checked":["x"]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	e.Repos = repos.Open(filepath.Join(tmp, "data"), e.Policy)
	rp, err := e.Repos.Add(mirror, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Repos.SetGitHub(rp.ID, "acme/reservations"); err != nil {
		t.Fatal(err)
	}
	// The mirror's directory is "mirror" but the fixture scenario keys on
	// "reservations": the task repo name is the directory basename, so tell
	// the scenario about it.
	repoName := filepath.Base(mirror)
	sc.Steps["researcher"] = executor.ScriptStep{Output: jfence(`{"summary":"x","uncertainty":"low","root_cause":{"statement":"dayKey keys by local day","file":"` + repoName + `/store.go","line":33}}`)}

	gh := fakegh.NewFakeGitHub()
	defer gh.Close()
	gh.SetPR("acme", "reservations", 7, baseSHA, shaA, "Fix duplicate reservation across timezones")
	client := &github.Client{Token: "t", BaseURL: gh.Server.URL}
	ctx := context.Background()

	// Webhook for A (delivered twice).
	var caseA *domain.Task
	for _, id := range []string{"d-A1", "d-A1"} {
		d, err := github.ParseDelivery("s", gh.WebhookRequest("s", id, "opened", "acme", "reservations", 7, "/github/webhook"))
		if err != nil {
			t.Fatal(err)
		}
		task, _, err := e.HandleDelivery(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		caseA = task
	}
	if cases, _ := e.CasesForPR("acme", "reservations", 7); len(cases) != 1 {
		t.Fatalf("%d cases after duplicate delivery", len(cases))
	}
	if caseA.PR.HeadSHA != shaA || caseA.HeadRef != shaA || caseA.State.PinnedBase != baseSHA {
		t.Fatalf("case not bound to PR identities: %+v", caseA.PR)
	}
	// Run with a repro command (set as if configured on the repository).
	caseA, _ = e.Store.GetTask(caseA.ID)
	caseA.ReproCommand = "go test -run " + tzTest + " ./..."
	if err := e.Store.SaveTask(caseA); err != nil {
		t.Fatal(err)
	}
	if err := e.RunTask(ctx, caseA.ID); err != nil {
		t.Fatal(err)
	}
	vA, _ := e.PacketState(caseA.ID)
	if vA.Packet.Verdict != domain.ClaimSupported || vA.Packet.Source.HeadSHAs[repoName] != shaA || vA.Packet.Source.BaseSHAs[repoName] != baseSHA {
		t.Fatalf("A: %s %s (%+v)", vA.Packet.Verdict, vA.Packet.VerdictWhy, vA.Packet.Source)
	}
	effs, err := e.PostGitHubStatus(ctx, client, caseA.ID, "http://pl/cases/"+caseA.ID)
	if err != nil || len(gh.Statuses) != 1 || gh.Statuses[0]["state"] != "success" || gh.Statuses[0]["sha"] != shaA || len(gh.Comments) != 1 {
		t.Fatalf("post A: %v effects=%v statuses=%v", err, effs, gh.Statuses)
	}
	// Posting again for the same packet version is a no-op (already done).
	if _, err := e.PostGitHubStatus(ctx, client, caseA.ID, "http://pl/cases/"+caseA.ID); err != nil || len(gh.Statuses) != 1 {
		t.Fatalf("second post duplicated: %v %d", err, len(gh.Statuses))
	}

	// Push B to the PR branch.
	gitRun(t, work, "checkout", "-q", "feature")
	os.WriteFile(filepath.Join(work, "store.go"), []byte(fixed+"\n// B\n"), 0o644)
	gitRun(t, work, "commit", "-qam", "B: follow-up")
	gitRun(t, work, "push", "-q", bare, "feature")
	shaB := strings.TrimSpace(gitOut(t, work, "rev-parse", "HEAD"))
	gh.SetPR("acme", "reservations", 7, baseSHA, shaB, "Fix duplicate reservation across timezones")

	// GitHub asks about B before anything verified it: never green.
	if st := github.BuildStatus(vA.Packet, repoName, shaB, "u"); st.State == "success" || !strings.Contains(st.Description, "NOT VERIFIED") {
		t.Fatalf("A's packet vouched for B: %+v", st)
	}
	// Three duplicate "synchronize" deliveries → one new case.
	var caseB *domain.Task
	for _, id := range []string{"d-B1", "d-B1", "d-B1"} {
		d, _ := github.ParseDelivery("s", gh.WebhookRequest("s", id, "synchronize", "acme", "reservations", 7, "/github/webhook"))
		task, _, err := e.HandleDelivery(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		caseB = task
	}
	cases, _ := e.CasesForPR("acme", "reservations", 7)
	if len(cases) != 2 || caseB.ID == caseA.ID || caseB.PR.HeadSHA != shaB {
		t.Fatalf("cases=%d B=%+v", len(cases), caseB.PR)
	}
	// A different delivery id for the same head does not create a third case.
	d, _ := github.ParseDelivery("s", gh.WebhookRequest("s", "d-B2", "synchronize", "acme", "reservations", 7, "/github/webhook"))
	if task, existing, _ := e.HandleDelivery(ctx, d); !existing || task.ID != caseB.ID {
		t.Fatal("same head SHA produced another case")
	}
	// A's packet is untouched history.
	vA2, _ := e.PacketState(caseA.ID)
	if vA2.Packet.Version != 1 || vA2.Packet.Verdict != domain.ClaimSupported || vA2.Packet.Source.HeadSHAs[repoName] != shaA {
		t.Fatalf("A changed: v%d %s", vA2.Packet.Version, vA2.Packet.Verdict)
	}
	// B is pending: its status must not be success either.
	vB, _ := e.PacketState(caseB.ID)
	if st := github.BuildStatus(vB.Packet, repoName, shaB, "u"); st.State == "success" {
		t.Fatalf("unverified B reported success: %+v", st)
	}

	// Revocation: refresh blocks the open case B, leaves done case A alone.
	gh.Revoked = true
	if _, err := e.RefreshPR(ctx, client, "acme", "reservations", 7); !errors.Is(err, github.ErrRevoked) {
		t.Fatalf("expected revoked, got %v", err)
	}
	gotB, _ := e.Store.GetTask(caseB.ID)
	gotA, _ := e.Store.GetTask(caseA.ID)
	if gotB.Status != domain.StatusFailed || !strings.Contains(gotB.FailureReason, "BLOCKED") || gotA.Status != domain.StatusDone {
		t.Fatalf("B=%s (%s) A=%s", gotB.Status, gotB.FailureReason, gotA.Status)
	}
	gh.Revoked = false

	// Effects ledger: simulate a crash after the POST but before "done" was
	// recorded — the retry must refuse rather than post twice.
	vA3, _ := e.PacketState(caseA.ID)
	_ = vA3
	effsA, _ := e.Store.Effects(caseA.ID)
	last := effsA[len(effsA)-1]
	last.Status = "pending"
	_ = e.Store.AddEffect(last)
	before := len(gh.Statuses) + len(gh.Comments)
	_, err = e.PostGitHubStatus(ctx, client, caseA.ID, "http://pl/cases/"+caseA.ID)
	if !errors.Is(err, ErrEffectUnknown) || len(gh.Statuses)+len(gh.Comments) != before {
		t.Fatalf("retry after crash: err=%v posts=%d→%d", err, before, len(gh.Statuses)+len(gh.Comments))
	}
	effsA, _ = e.Store.Effects(caseA.ID)
	if effsA[len(effsA)-1].Status != "unknown" {
		t.Fatalf("effect not marked unknown: %+v", effsA[len(effsA)-1])
	}
}
