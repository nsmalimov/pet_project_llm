package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"orchestrator/internal/domain"
	"orchestrator/internal/executor"
)

// fixtureRepo instantiates fixtures/reservations (real timezone duplicate
// bug) as a git repo named "reservations".
func fixtureRepo(t *testing.T, root string) string {
	t.Helper()
	src := filepath.Join("..", "..", "fixtures", "reservations")
	dir := filepath.Join(root, "reservations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "test@test")
	gitRun(t, dir, "config", "user.name", "test")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "buggy")
	return dir
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "reservations", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func claimByType(p *domain.Packet, typ domain.ClaimType) domain.Claim {
	for _, c := range p.Claims {
		if c.Type == typ {
			return c
		}
	}
	return domain.Claim{}
}

// The acceptance scenario: real bug → baseline reproduces → fix → real tests
// pass → independent review → packet from persisted artifacts only → one
// deliberately unchecked aspect stays INSUFFICIENT → human verdict persists.
func TestProoflineVerticalSlice(t *testing.T) {
	tmp := t.TempDir()
	repo := fixtureRepo(t, tmp)
	fixed := replaceOnce(t, readFixture(t, "store.go"),
		`return room + "/" + day.Format("2006-01-02")`,
		`return room + "/" + day.UTC().Format("2006-01-02")`)
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"dayKey formats the caller's local time; the UTC day is not normalised.","key_files":["reservations/store.go"],"uncertainty":"low","risks":["handler.go passes client zones straight through"],"root_cause":{"statement":"dayKey builds the slot key from the un-normalised local time, so equal UTC days in different zones get different keys","file":"reservations/store.go","line":33}}`)},
		"developer":  devStep("completed", "normalise to UTC in dayKey", []string{"reservations/store.go", fixed}),
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"UTC normalisation closes the gap; key derivation is the only site.","findings":[{"severity":"low","file":"reservations/store.go","issue":"stored Day keeps the caller's zone"}],"checked":["Reserve key derivation","existing tests"],"not_checked":["HTTP handler path","concurrent callers"],"counterexample":""}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))

	task, err := e.CreateTaskSpec(TaskSpec{
		Goal:         "Fix duplicate reservation accepted for the same UTC day across timezones",
		Repos:        []string{repo},
		ReproCommand: "go test -run TestReserveRejectsSameUTCDayAcrossTimezones ./...",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Kind != domain.KindBugfix {
		t.Fatalf("kind=%s, want bugfix", task.Kind)
	}
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}

	v, err := e.PacketState(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Task.Status != domain.StatusDone {
		t.Fatalf("status=%s (%s)", v.Task.Status, v.Task.FailureReason)
	}
	p := v.Packet

	// Baseline really ran on the untouched code and really failed.
	var baseline, after, diff, review int
	for _, a := range v.Artifacts {
		switch a.Kind {
		case domain.ArtBaselineRun:
			baseline++
			if a.Passed == nil || *a.Passed {
				t.Fatalf("baseline %q should fail on the buggy code", a.Command)
			}
			if a.Output == "" {
				t.Fatal("baseline output not captured")
			}
		case domain.ArtTestRun:
			after++
			if a.Passed == nil || !*a.Passed {
				t.Fatalf("after-change run %q should pass: %s", a.Command, a.Output)
			}
		case domain.ArtDiff:
			diff++
			if len(a.Commits) == 0 || a.Diff == "" {
				t.Fatalf("diff artifact lacks commit/diff: %+v", a.Commits)
			}
		case domain.ArtReview:
			review++
		}
	}
	if baseline != 2 || after != 2 || diff != 1 || review != 1 {
		t.Fatalf("artifacts baseline=%d after=%d diff=%d review=%d", baseline, after, diff, review)
	}

	want := map[domain.ClaimType]domain.ClaimStatus{
		domain.ClaimProblemReproduced:    domain.ClaimSupported,
		domain.ClaimRootCauseSupported:   domain.ClaimSupported,
		domain.ClaimChangeVerified:       domain.ClaimSupported,
		domain.ClaimIndependentChallenge: domain.ClaimSupported,
		domain.ClaimIntegrationChecked:   domain.ClaimInsufficient, // deliberately unchecked
		domain.ClaimCrossServiceImpact:   domain.ClaimInsufficient,
	}
	for typ, st := range want {
		c := claimByType(p, typ)
		if c.Status != st {
			t.Fatalf("claim %s = %s (%s), want %s", typ, c.Status, c.Reason, st)
		}
		if st == domain.ClaimSupported && len(c.ArtifactIDs) == 0 {
			t.Fatalf("supported claim %s has no artifacts", typ)
		}
	}
	if p.Verdict != domain.ClaimSupported {
		t.Fatalf("verdict=%s: %s", p.Verdict, p.VerdictWhy)
	}
	if len(p.Gaps) < 3 { // 2 unchecked claims + reviewer not_checked
		t.Fatalf("gaps must stay visible, got %v", p.Gaps)
	}
	if v.Evidence[len(v.Evidence)-1].Level == "" || !hasLevel(v.Evidence, domain.EvidenceReproduced) {
		t.Fatal("reproduced evidence level not emitted")
	}

	// Human decision persists and pins the packet version.
	if _, err := e.RecordVerdict(task.ID, "request_changes", "handler path unverified", "tester"); err != nil {
		t.Fatal(err)
	}
	// Re-open the store from disk: everything must survive a "refresh".
	e2 := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	v2, err := e2.PacketState(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Packet.Version != p.Version || v2.Packet.Fingerprint != p.Fingerprint {
		t.Fatalf("packet changed on rebuild: v%d/%s vs v%d/%s", v2.Packet.Version, v2.Packet.Fingerprint, p.Version, p.Fingerprint)
	}
	if len(v2.Verdicts) != 1 || v2.Verdicts[0].Decision != "request_changes" || v2.Verdicts[0].PacketVersion != p.Version {
		t.Fatalf("verdict not persisted: %+v", v2.Verdicts)
	}
	// The original repository is untouched.
	if readFile(t, filepath.Join(repo, "store.go")) != readFixture(t, "store.go") {
		t.Fatal("original repo modified")
	}
}

// A test that passes on the baseline cannot prove reproduction — the packet
// must say CONTRADICTED, and tests passing later must not count as verified.
func TestPacketRefusesReproductionWhenBaselinePasses(t *testing.T) {
	tmp := t.TempDir()
	repo := fixtureRepo(t, tmp)
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","key_files":["reservations/store.go"],"uncertainty":"low"}`)},
		"developer":  devStep("completed", "noop edit", []string{"reservations/store.go", readFixture(t, "store.go") + "\n// touched\n"}),
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, err := e.CreateTaskSpec(TaskSpec{
		Goal: "Fix duplicate reservation", Repos: []string{repo},
		// Both commands only run the tests that already pass.
		TestCommand:  "go test -run 'TestReserveRejectsExactDuplicate|TestReserveDifferentDaysAndRooms' ./...",
		ReproCommand: "go test -run TestReserveRejectsExactDuplicate ./...",
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
	if v.Task.Status != domain.StatusDone {
		t.Fatalf("status=%s", v.Task.Status)
	}
	if c := claimByType(v.Packet, domain.ClaimProblemReproduced); c.Status != domain.ClaimContradicted {
		t.Fatalf("reproduced=%s, want contradicted (%s)", c.Status, c.Reason)
	}
	if c := claimByType(v.Packet, domain.ClaimChangeVerified); c.Status != domain.ClaimInsufficient {
		t.Fatalf("change_verified=%s, want insufficient (%s)", c.Status, c.Reason)
	}
	if v.Packet.Verdict != domain.ClaimBlocked {
		t.Fatalf("verdict=%s, want blocked: %s", v.Packet.Verdict, v.Packet.VerdictWhy)
	}
	if _, err := e.RecordVerdict(task.ID, "merge", "", ""); err == nil {
		t.Fatal("invalid decision accepted")
	}
}

func hasLevel(evs []domain.Evidence, l domain.EvidenceLevel) bool {
	for _, e := range evs {
		if e.Level == l {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func replaceOnce(t *testing.T, s, old, new string) string {
	t.Helper()
	i := indexOf(s, old)
	if i < 0 {
		t.Fatalf("fixture does not contain %q", old)
	}
	return s[:i] + new + s[i+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
