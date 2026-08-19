package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"orchestrator/internal/domain"
	"orchestrator/internal/executor"
	"orchestrator/internal/gitws"
	"orchestrator/internal/router"
	"orchestrator/internal/store"
)

// ---------- helpers ----------

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// makeRepo creates a git repo with a buggy Add (returns a-b) and a test
// expecting correct addition.
func makeRepo(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":      "module example.com/" + name + "\n\ngo 1.21\n",
		"add.go":      "package mathx\n\nfunc Add(a, b int) int { return a - b }\n",
		"add_test.go": "package mathx\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatalf(\"Add(2,3)=%d\", Add(2, 3))\n\t}\n}\n",
	}
	for f, c := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "test@test")
	gitRun(t, dir, "config", "user.name", "test")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

const fixedAdd = "package mathx\n\nfunc Add(a, b int) int { return a + b }\n"
const stillBroken = "package mathx\n\nfunc Add(a, b int) int { return a * b }\n"

func jfence(s string) string { return "```json\n" + s + "\n```" }

func newTestEngine(t *testing.T, sc *executor.ScriptExecutor, dataDir string) *Engine {
	t.Helper()
	st, err := store.NewFileStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	execs := map[string]executor.Executor{"mock": sc}
	rt := router.Rules{Executor: "mock", CheapModel: "sonnet", StrongModel: "opus"}
	ws := gitws.NewManager(filepath.Join(dataDir, "worktrees"))
	cfg := DefaultConfig()
	return New(st, ws, execs, rt, nil, cfg)
}

func eventTypes(t *testing.T, e *Engine, taskID string) []string {
	t.Helper()
	evs, err := e.Store.Events(taskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, len(evs))
	for i, ev := range evs {
		types[i] = ev.Type
	}
	return types
}

func hasEvent(types []string, want string) bool {
	for _, ty := range types {
		if ty == want {
			return true
		}
	}
	return false
}

func devStep(status, summary string, files []string) executor.ScriptStep {
	return executor.ScriptStep{
		Output: jfence(`{"status":"` + status + `","summary":"` + summary + `","files_changed":["repoA/add.go"]}`),
		Files:  fileWrites(files),
	}
}

func fileWrites(files []string) []executor.ScriptFile {
	var out []executor.ScriptFile
	for i := 0; i+1 < len(files); i += 2 {
		out = append(out, executor.ScriptFile{Path: files[i], Content: files[i+1]})
	}
	return out
}

// ---------- tests ----------

func TestHappyPathEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"Add subtracts instead of adding","key_files":["repoA/add.go"],"uncertainty":"low"}`)},
		"developer":  devStep("completed", "fixed Add", []string{"repoA/add.go", fixedAdd}),
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"correct and minimal","findings":[]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))

	task, err := e.CreateTask("Fix the Add function", nil, []string{repo}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}

	fs, err := e.FullState(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fs.Task.Status != domain.StatusDone {
		t.Fatalf("status=%s, failure=%s", fs.Task.Status, fs.Task.FailureReason)
	}
	if fs.Confidence != string(domain.EvidenceReviewed) {
		t.Fatalf("confidence=%s, want reviewed", fs.Confidence)
	}
	types := eventTypes(t, e, task.ID)
	for _, want := range []string{
		domain.EvTaskCreated, domain.EvWorkspacePrepared, domain.EvRouteChosen,
		domain.EvFilesChanged, domain.EvTestsStarted, domain.EvTestsPassed,
		domain.EvReviewStarted, domain.EvReviewCompleted, domain.EvTaskCompleted,
	} {
		if !hasEvent(types, want) {
			t.Fatalf("missing event %s in %v", want, types)
		}
	}
	// Original repo must be untouched.
	b, _ := os.ReadFile(filepath.Join(repo, "add.go"))
	if string(b) != "package mathx\n\nfunc Add(a, b int) int { return a - b }\n" {
		t.Fatal("original working tree was modified")
	}
	// Worktree must contain the fix.
	wb, _ := os.ReadFile(filepath.Join(fs.Task.State.WorktreeRoot, "repoA", "add.go"))
	if string(wb) != fixedAdd {
		t.Fatal("worktree does not contain the fix")
	}
	if len(fs.Runs) < 3 {
		t.Fatalf("expected >=3 agent runs, got %d", len(fs.Runs))
	}
	for _, r := range fs.Runs {
		if r.Status != "ok" || r.RouteReason == "" {
			t.Fatalf("run not observable: %+v", r)
		}
	}
}

func TestFailingTestsLoopBackToImplementation(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher":  {Output: jfence(`{"summary":"bug in Add","uncertainty":"low"}`)},
		"developer:0": devStep("completed", "attempt 1", []string{"repoA/add.go", stillBroken}),
		"developer:1": devStep("completed", "attempt 2", []string{"repoA/add.go", fixedAdd}),
		"reviewer":    {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTask("Fix Add", nil, []string{repo}, "")
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	fs, _ := e.FullState(task.ID)
	if fs.Task.Status != domain.StatusDone {
		t.Fatalf("status=%s (%s)", fs.Task.Status, fs.Task.FailureReason)
	}
	if fs.Task.State.FixAttempts != 1 {
		t.Fatalf("FixAttempts=%d, want 1", fs.Task.State.FixAttempts)
	}
	types := eventTypes(t, e, task.ID)
	if !hasEvent(types, domain.EvTestsFailed) || !hasEvent(types, domain.EvTestsPassed) {
		t.Fatalf("expected fail-then-pass, events: %v", types)
	}
}

func TestHighUncertaintyTriggersInvestigation(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher:0": {Output: jfence(`{"summary":"unclear semantics","uncertainty":"high","open_questions":["what should Add do with overflow?"]}`)},
		"researcher:1": {Output: jfence(`{"summary":"overflow is out of scope; plain addition","uncertainty":"low"}`)},
		"developer":    devStep("completed", "fixed", []string{"repoA/add.go", fixedAdd}),
		"reviewer":     {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTask("Fix Add", nil, []string{repo}, "")
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	fs, _ := e.FullState(task.ID)
	if fs.Task.Status != domain.StatusDone {
		t.Fatalf("status=%s (%s)", fs.Task.Status, fs.Task.FailureReason)
	}
	if fs.Task.State.Investigations != 1 {
		t.Fatalf("Investigations=%d, want 1", fs.Task.State.Investigations)
	}
}

func TestReviewChangesRequestedLoop(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher":  {Output: jfence(`{"summary":"bug","uncertainty":"low"}`)},
		"developer:0": devStep("completed", "fix v1", []string{"repoA/add.go", fixedAdd}),
		"developer:1": {
			Output: jfence(`{"status":"completed","summary":"added comment per review","files_changed":["repoA/add.go"]}`),
			Files:  []executor.ScriptFile{{Path: "repoA/add.go", Content: "package mathx\n\n// Add returns the sum of a and b.\nfunc Add(a, b int) int { return a + b }\n"}},
		},
		"reviewer:0": {Output: jfence(`{"verdict":"changes_requested","summary":"exported func needs a doc comment","findings":[{"severity":"low","file":"repoA/add.go","issue":"missing doc comment"}]}`)},
		"reviewer:1": {Output: jfence(`{"verdict":"approve","summary":"ok now","findings":[]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTask("Fix Add", nil, []string{repo}, "")
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	fs, _ := e.FullState(task.ID)
	if fs.Task.Status != domain.StatusDone {
		t.Fatalf("status=%s (%s)", fs.Task.Status, fs.Task.FailureReason)
	}
	if fs.Task.State.ReviewRounds != 1 {
		t.Fatalf("ReviewRounds=%d, want 1", fs.Task.State.ReviewRounds)
	}
	types := eventTypes(t, e, task.ID)
	if !hasEvent(types, domain.EvReviewFinding) {
		t.Fatal("missing review.finding event")
	}
}

func TestBlockedDeveloperCreatesDecisionAndResumes(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"two possible fixes","uncertainty":"low"}`)},
		"developer:0": {Output: jfence(`{"status":"blocked","summary":"ambiguous requirement","notes":"n/a",
			"decision_request":{"importance":"high","question":"Which behaviour is intended?","recommendation":"plain addition","reason":"tests imply it","options":[{"id":"add","label":"Plain addition"},{"id":"sat","label":"Saturating addition"}]}}`)},
		"developer:1": devStep("completed", "implemented per decision", []string{"repoA/add.go", fixedAdd}),
		"reviewer":    {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTask("Fix Add", nil, []string{repo}, "")
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}

	fs, _ := e.FullState(task.ID)
	if fs.Task.Status != domain.StatusAwaitingDecision {
		t.Fatalf("status=%s, want awaiting_decision", fs.Task.Status)
	}
	if len(fs.Decisions) != 1 || fs.Decisions[0].Status != "open" {
		t.Fatalf("decisions: %+v", fs.Decisions)
	}
	d := fs.Decisions[0]
	if d.Importance != "high" || len(d.Options) != 2 {
		t.Fatalf("decision malformed: %+v", d)
	}

	if _, err := e.ResolveDecision(task.ID, d.ID, "add", "keep it simple"); err != nil {
		t.Fatal(err)
	}
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	fs, _ = e.FullState(task.ID)
	if fs.Task.Status != domain.StatusDone {
		t.Fatalf("status=%s (%s)", fs.Task.Status, fs.Task.FailureReason)
	}
	found := false
	for _, n := range fs.Task.State.Notes {
		if n != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("decision resolution not recorded in task notes")
	}
}

func TestAbortDecisionFailsTask(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low","decision_request":{"importance":"high","question":"proceed?","options":[{"id":"go","label":"go"},{"id":"abort","label":"stop"}]}}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTask("Fix Add", nil, []string{repo}, "")
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	fs, _ := e.FullState(task.ID)
	d := fs.Decisions[0]
	if _, err := e.ResolveDecision(task.ID, d.ID, "abort", ""); err != nil {
		t.Fatal(err)
	}
	fs, _ = e.FullState(task.ID)
	if fs.Task.Status != domain.StatusFailed {
		t.Fatalf("status=%s, want failed", fs.Task.Status)
	}
}

func TestRecoverAndResumeAfterCrash(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"bug","uncertainty":"low"}`)},
		"developer":  devStep("completed", "fixed", []string{"repoA/add.go", fixedAdd}),
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[]}`)},
	}}
	dataDir := filepath.Join(tmp, "data")
	e := newTestEngine(t, sc, dataDir)
	task, _ := e.CreateTask("Fix Add", nil, []string{repo}, "")

	// Simulate a crash mid-implementation: status persisted as implementing.
	tk, _ := e.Store.GetTask(task.ID)
	tk.Status = domain.StatusImplementing
	if err := e.Store.SaveTask(tk); err != nil {
		t.Fatal(err)
	}

	// New process: recover, resume, run to completion.
	e2 := newTestEngine(t, sc, dataDir)
	if err := e2.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	tk, _ = e2.Store.GetTask(task.ID)
	if tk.Status != domain.StatusInterrupted {
		t.Fatalf("status=%s, want interrupted", tk.Status)
	}
	if _, err := e2.Resume(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := e2.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	fs, _ := e2.FullState(task.ID)
	if fs.Task.Status != domain.StatusDone {
		t.Fatalf("status=%s (%s)", fs.Task.Status, fs.Task.FailureReason)
	}
}

func TestMultiRepoWorkspace(t *testing.T) {
	tmp := t.TempDir()
	repoA := makeRepo(t, tmp, "repoA")
	repoB := makeRepo(t, tmp, "repoB")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"fix both","uncertainty":"low"}`)},
		"developer": {
			Output: jfence(`{"status":"completed","summary":"fixed both repos","files_changed":["repoA/add.go","repoB/add.go"]}`),
			Files: []executor.ScriptFile{
				{Path: "repoA/add.go", Content: fixedAdd},
				{Path: "repoB/add.go", Content: fixedAdd},
			},
		},
		"reviewer": {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, err := e.CreateTask("Fix Add in both repos", nil, []string{repoA, repoB}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	fs, _ := e.FullState(task.ID)
	if fs.Task.Status != domain.StatusDone {
		t.Fatalf("status=%s (%s)", fs.Task.Status, fs.Task.FailureReason)
	}
	if len(fs.Task.State.ChangedFiles) != 2 {
		t.Fatalf("changed files: %v", fs.Task.State.ChangedFiles)
	}
}

func TestAgentFailurePausesOnDecisionInsteadOfFailing(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"bug","uncertainty":"low"}`)},
		"developer":  {Fail: "simulated executor timeout"},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTask("Fix Add", nil, []string{repo}, "")
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	fs, _ := e.FullState(task.ID)
	if fs.Task.Status != domain.StatusAwaitingDecision {
		t.Fatalf("status=%s, want awaiting_decision (work must not be discarded)", fs.Task.Status)
	}
	if len(fs.Decisions) != 1 || fs.Decisions[0].Status != "open" {
		t.Fatalf("expected one open decision, got %+v", fs.Decisions)
	}
	// Research results must survive the failure.
	if fs.Task.State.ResearchSummary == "" {
		t.Fatal("research summary lost")
	}
	if _, err := e.ResolveDecision(task.ID, fs.Decisions[0].ID, "abort", ""); err != nil {
		t.Fatal(err)
	}
	fs, _ = e.FullState(task.ID)
	if fs.Task.Status != domain.StatusFailed {
		t.Fatalf("status=%s, want failed after abort", fs.Task.Status)
	}
}

func TestAcceptDecisionOverridesReviewer(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"bug","uncertainty":"low"}`)},
		"developer":  devStep("completed", "fixed", []string{"repoA/add.go", fixedAdd}),
		"reviewer":   {Output: jfence(`{"verdict":"changes_requested","summary":"style nit","findings":[{"severity":"low","file":"repoA/add.go","issue":"nit"}]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	e.Cfg.MaxReviewRounds = 0 // first changes_requested escalates to a decision
	task, _ := e.CreateTask("Fix Add", nil, []string{repo}, "")
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	fs, _ := e.FullState(task.ID)
	if fs.Task.Status != domain.StatusAwaitingDecision {
		t.Fatalf("status=%s, want awaiting_decision", fs.Task.Status)
	}
	if _, err := e.ResolveDecision(task.ID, fs.Decisions[0].ID, "accept", "nit is fine"); err != nil {
		t.Fatal(err)
	}
	fs, _ = e.FullState(task.ID)
	if fs.Task.Status != domain.StatusDone {
		t.Fatalf("status=%s, want done after accept", fs.Task.Status)
	}
	// Confidence must honestly stay at tested, not reviewed.
	if fs.Confidence != string(domain.EvidenceTested) {
		t.Fatalf("confidence=%s, want tested", fs.Confidence)
	}
}

func TestResolveDecisionGuardsTaskStatus(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low","decision_request":{"importance":"high","question":"q?","options":[{"id":"a","label":"A"},{"id":"abort","label":"stop"}]}}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTask("Fix Add", nil, []string{repo}, "")
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	fs, _ := e.FullState(task.ID)
	d := fs.Decisions[0]
	if _, err := e.ResolveDecision(task.ID, d.ID, "abort", ""); err != nil {
		t.Fatal(err)
	}
	// Task is now failed; resolving again (stale decision) must be rejected.
	if _, err := e.ResolveDecision(task.ID, d.ID, "a", ""); err == nil {
		t.Fatal("resolving a decision on a non-awaiting task must fail")
	}
}
