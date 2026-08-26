package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orchestrator/internal/domain"
	"orchestrator/internal/executor"
)

// P1 scenario matrix: every scenario runs the real engine (worktrees, real
// `go test`, real commits) on the reservations fixture with a scripted
// developer/reviewer. No verdict is hardcoded — the packet is derived from
// what actually happened. Set PROOFLINE_WRITE_MATRIX=1 to (re)generate
// docs/SCENARIOS.md from the actual outcomes.

type scenario struct {
	Name, Desc     string
	Repro          string
	Kind           domain.TaskKind
	Steps          map[string]executor.ScriptStep
	Cfg            func(*Config)
	Prepare        func(t *testing.T, repo string) // mutate the fixture before commit
	Expect         domain.ClaimStatus
	ExpectClaims   map[domain.ClaimType]domain.ClaimStatus
	ExpectContains string // must appear in contradictions/gaps/risks
}

const tzTest = "TestReserveRejectsSameUTCDayAcrossTimezones"

func scenarios(t *testing.T) []scenario {
	orig := readFixture(t, "store.go")
	origTest := readFixture(t, "store_test.go")
	fixed := replaceOnce(t, orig, `day.Format("2006-01-02")`, `day.UTC().Format("2006-01-02")`)
	regress := replaceOnce(t, orig, `return room + "/" + day.Format("2006-01-02")`, `return room // BUG: collapses every day into one slot`)
	fakeTest := replaceOnce(t, origTest,
		`if _, err := s.Reserve("101", utc); !errors.Is(err, ErrDuplicate) {`,
		`if _, err := s.Reserve("101", utc); err != nil { // "fixed" by inverting the expectation`)
	skipTest := replaceOnce(t, origTest, `func TestReserveRejectsSameUTCDayAcrossTimezones(t *testing.T) {`,
		`func TestReserveRejectsSameUTCDayAcrossTimezones(t *testing.T) {
	t.Skip("temporarily disabled")`)
	research := `{"summary":"dayKey formats local time","key_files":["reservations/store.go"],"uncertainty":"low","root_cause":{"statement":"dayKey keys by un-normalised local day","file":"reservations/store.go","line":33}}`
	approve := `{"verdict":"approve","summary":"ok","findings":[],"checked":["Reserve key derivation","all three tests traced"],"not_checked":["HTTP handler","concurrency"]}`
	repro := "go test -run " + tzTest + " ./..."

	return []scenario{
		{
			Name: "A", Desc: "valid bugfix: baseline fails → fix → regression passes → challenger finds nothing",
			Repro: repro,
			Steps: map[string]executor.ScriptStep{
				"researcher": {Output: jfence(research)},
				"developer":  devStep("completed", "utc", []string{"reservations/store.go", fixed}),
				"reviewer":   {Output: jfence(approve)},
			},
			Expect: domain.ClaimSupported,
			ExpectClaims: map[domain.ClaimType]domain.ClaimStatus{
				domain.ClaimProblemReproduced: domain.ClaimSupported, domain.ClaimChangeVerified: domain.ClaimSupported,
				domain.ClaimIndependentChallenge: domain.ClaimSupported, domain.ClaimIntegrationChecked: domain.ClaimInsufficient,
			},
		},
		{
			Name: "B1", Desc: "fake fix: developer inverts the failing assertion instead of fixing the code; suite passes",
			Repro: repro,
			Steps: map[string]executor.ScriptStep{
				"researcher": {Output: jfence(research)},
				"developer":  devStep("completed", "fixed test", []string{"reservations/store_test.go", fakeTest}),
				"reviewer":   {Output: jfence(approve)},
			},
			Expect:         domain.ClaimBlocked,
			ExpectClaims:   map[domain.ClaimType]domain.ClaimStatus{domain.ClaimChangeVerified: domain.ClaimContradicted},
			ExpectContains: "ORIGINAL tests fail",
		},
		{
			Name: "B2", Desc: "fake fix: developer skips the failing test; exit 0, nothing proven",
			Repro: repro,
			Steps: map[string]executor.ScriptStep{
				"researcher": {Output: jfence(research)},
				"developer":  devStep("completed", "skip", []string{"reservations/store_test.go", skipTest}),
				"reviewer":   {Output: jfence(approve)},
			},
			Expect:       domain.ClaimBlocked, // original test replay fails → contradicted
			ExpectClaims: map[domain.ClaimType]domain.ClaimStatus{domain.ClaimChangeVerified: domain.ClaimContradicted},
		},
		{
			Name: "C", Desc: "regression: the bug is fixed but another behaviour breaks (all days collapse into one slot)",
			Repro: repro,
			Steps: map[string]executor.ScriptStep{
				"researcher": {Output: jfence(research)},
				"developer":  devStep("completed", "key by room", []string{"reservations/store.go", regress}),
				"reviewer":   {Output: jfence(approve)},
			},
			Cfg:            func(c *Config) { c.MaxFixAttempts = 0 },
			Expect:         domain.ClaimBlocked,
			ExpectClaims:   map[domain.ClaimType]domain.ClaimStatus{domain.ClaimChangeVerified: domain.ClaimContradicted},
			ExpectContains: "TestReserveDifferentDaysAndRooms",
		},
		{
			Name: "D", Desc: "insufficient verification: no tests exist, nothing can be reproduced or verified; review approves",
			Prepare: func(t *testing.T, repo string) {
				if err := os.Remove(filepath.Join(repo, "store_test.go")); err != nil {
					t.Fatal(err)
				}
			},
			Steps: map[string]executor.ScriptStep{
				"researcher": {Output: jfence(research)},
				"developer":  devStep("completed", "utc", []string{"reservations/store.go", fixed}),
				"reviewer":   {Output: jfence(approve)},
			},
			Expect: domain.ClaimInsufficient,
			ExpectClaims: map[domain.ClaimType]domain.ClaimStatus{
				domain.ClaimProblemReproduced: domain.ClaimInsufficient, domain.ClaimChangeVerified: domain.ClaimInsufficient,
			},
		},
		{
			Name: "E", Desc: "reviewer counterexample: fix + tests look right, independent challenger finds a concrete breaking scenario",
			Repro: repro,
			Steps: map[string]executor.ScriptStep{
				"researcher": {Output: jfence(research)},
				"developer":  devStep("completed", "utc", []string{"reservations/store.go", fixed}),
				"reviewer":   {Output: jfence(`{"verdict":"changes_requested","summary":"UTC-day keying still double-books one local day","findings":[{"severity":"high","file":"reservations/store.go","issue":"09:00-04:00 and 23:30-04:00 on the same New York day map to different UTC days and both succeed"}],"checked":["key derivation"],"not_checked":["handler"],"counterexample":"Reserve(\"101\", 2026-03-14T09:00-04:00) then Reserve(\"101\", 2026-03-14T23:30-04:00) both succeed"}`)},
			},
			Cfg:            func(c *Config) { c.MaxReviewRounds = 0 },
			Expect:         domain.ClaimBlocked,
			ExpectClaims:   map[domain.ClaimType]domain.ClaimStatus{domain.ClaimIndependentChallenge: domain.ClaimContradicted, domain.ClaimChangeVerified: domain.ClaimSupported},
			ExpectContains: "Counterexample",
		},
		{
			Name: "F", Desc: "stale evidence: verified change, then the code changes again after verification",
			Repro: repro,
			Steps: map[string]executor.ScriptStep{
				"researcher": {Output: jfence(research)},
				"developer":  devStep("completed", "utc", []string{"reservations/store.go", fixed}),
				"reviewer":   {Output: jfence(approve)},
			},
			Expect: domain.ClaimStale, // applied after the run, see runScenario
			ExpectClaims: map[domain.ClaimType]domain.ClaimStatus{
				domain.ClaimChangeVerified: domain.ClaimStale, domain.ClaimIndependentChallenge: domain.ClaimStale,
			},
		},
	}
}

type matrixRow struct {
	Scenario, Verdict, Expected, Claims, Gaps, Contra string
}

func runScenario(t *testing.T, sc scenario) matrixRow {
	tmp := t.TempDir()
	dir := fixtureRepo(t, tmp)
	if sc.Prepare != nil {
		sc.Prepare(t, dir)
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "prepared")
	}

	e := newTestEngine(t, &executor.ScriptExecutor{Steps: sc.Steps}, filepath.Join(tmp, "data"))
	if sc.Cfg != nil {
		sc.Cfg(&e.Cfg)
	}
	task, err := e.CreateTaskSpec(TaskSpec{Goal: "Fix duplicate reservation across timezones", Repos: []string{dir}, ReproCommand: sc.Repro, Kind: sc.Kind})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if sc.Name == "F" {
		// Someone edits the worktree after verification and commits.
		wt := filepath.Join(e.WS.Root, task.ID, "reservations", "store.go")
		b, _ := os.ReadFile(wt)
		if err := os.WriteFile(wt, append(b, []byte("\n// post-verification edit\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, filepath.Dir(wt), "commit", "-qam", "late edit")
	}
	v, err := e.PacketState(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := v.Packet
	var claims []string
	for _, c := range p.Claims {
		claims = append(claims, fmt.Sprintf("%s=%s", c.Type, c.Status))
	}
	row := matrixRow{Scenario: sc.Name + " — " + sc.Desc, Verdict: string(p.Verdict), Expected: string(sc.Expect),
		Claims: strings.Join(claims, "<br>"), Gaps: fmt.Sprint(len(p.Gaps)), Contra: strings.Join(p.Contradictions, "<br>")}

	if p.Verdict != sc.Expect {
		t.Errorf("scenario %s: verdict %s, want %s (%s)\nclaims: %s", sc.Name, p.Verdict, sc.Expect, p.VerdictWhy, strings.Join(claims, "\n"))
	}
	for typ, want := range sc.ExpectClaims {
		if got := claimByType(p, typ); got.Status != want {
			t.Errorf("scenario %s: %s = %s (%s), want %s", sc.Name, typ, got.Status, got.Reason, want)
		}
	}
	if sc.ExpectContains != "" {
		all := strings.Join(p.Contradictions, "\n") + strings.Join(p.Gaps, "\n")
		for _, r := range p.Risks {
			all += "\n" + r.Text
		}
		for _, c := range p.Claims {
			all += "\n" + c.Statement + "\n" + c.Reason
		}
		if !strings.Contains(all, sc.ExpectContains) {
			t.Errorf("scenario %s: packet does not surface %q", sc.Name, sc.ExpectContains)
		}
	}
	// Every SUPPORTED claim must point at artifacts on the current state.
	for _, c := range p.Claims {
		if c.Status == domain.ClaimSupported && len(c.ArtifactIDs) == 0 {
			t.Errorf("scenario %s: supported claim %s without artifacts", sc.Name, c.Type)
		}
	}
	return row
}

func TestScenarioMatrix(t *testing.T) {
	var rows []matrixRow
	for _, sc := range scenarios(t) {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) { rows = append(rows, runScenario(t, sc)) })
	}
	if os.Getenv("PROOFLINE_WRITE_MATRIX") == "" {
		return
	}
	var sb strings.Builder
	sb.WriteString("# Scenario matrix (generated by `PROOFLINE_WRITE_MATRIX=1 go test ./internal/engine -run TestScenarioMatrix`)\n\n")
	sb.WriteString("Every row is a real engine run: git worktree, real `go test -v`, real commits, scripted developer/reviewer. Verdicts are derived, never hardcoded.\n\n")
	sb.WriteString("| scenario | expected | actual | claims | gaps | contradictions |\n|---|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "| %s | **%s** | **%s** | %s | %s | %s |\n", r.Scenario, strings.ToUpper(r.Expected), strings.ToUpper(r.Verdict), r.Claims, r.Gaps, r.Contra)
	}
	if err := os.WriteFile(filepath.Join("..", "..", "docs", "SCENARIOS.md"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
