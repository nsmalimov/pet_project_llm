package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"orchestrator/internal/domain"
	"orchestrator/internal/executor"
	"orchestrator/internal/store"
)

// P2 — concurrency and consistency invariants.

// B: the human decides on packet v1; a rerun/edit produces v2 before the
// decision lands. The decision must be refused, never attached to v2.
func TestVerdictPinnedToViewedPacketVersion(t *testing.T) {
	tmp := t.TempDir()
	repo := fixtureRepo(t, tmp)
	fixed := replaceOnce(t, readFixture(t, "store.go"), `day.Format("2006-01-02")`, `day.UTC().Format("2006-01-02")`)
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low","root_cause":{"statement":"dayKey keys by local day","file":"reservations/store.go","line":33}}`)},
		"developer":  devStep("completed", "utc", []string{"reservations/store.go", fixed}),
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[],"checked":["x"]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTaskSpec(TaskSpec{Goal: "Fix duplicate reservation", Repos: []string{repo}, ReproCommand: "go test -run " + tzTest + " ./..."})
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	v1, _ := e.PacketState(task.ID)
	if v1.Packet.Version != 1 || v1.Packet.Verdict != domain.ClaimSupported {
		t.Fatalf("v%d %s", v1.Packet.Version, v1.Packet.Verdict)
	}
	// The code changes (v2 will be STALE) while the human still looks at v1.
	wt := filepath.Join(v1.Task.State.WorktreeRoot, "reservations", "store.go")
	b, _ := os.ReadFile(wt)
	os.WriteFile(wt, append(b, []byte("\n// late\n")...), 0o644)
	gitRun(t, filepath.Dir(wt), "commit", "-qam", "late")

	_, err := e.RecordVerdict(task.ID, "accept", "looks good", "human", 1)
	if !errors.Is(err, ErrPacketChanged) {
		t.Fatalf("accept for v1 must be refused once v2 exists, got %v", err)
	}
	vs, _ := e.Store.Verdicts(task.ID)
	if len(vs) != 0 {
		t.Fatal("no verdict may be stored")
	}
	v2, _ := e.PacketState(task.ID)
	if v2.Packet.Version != 2 || v2.Packet.Verdict != domain.ClaimStale {
		t.Fatalf("v%d %s", v2.Packet.Version, v2.Packet.Verdict)
	}
	// Deciding on what is actually current works and pins v2.
	ver, err := e.RecordVerdict(task.ID, "request_changes", "re-verify", "human", 2)
	if err != nil || ver.PacketVersion != 2 {
		t.Fatalf("%v %+v", err, ver)
	}
}

// C: the same creation request replayed three times yields one task.
func TestIdempotentCreation(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	e := newTestEngine(t, &executor.ScriptExecutor{}, filepath.Join(tmp, "data"))
	var wg sync.WaitGroup
	ids := make(chan string, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, _, err := e.CreateTaskIdempotent(TaskSpec{Goal: "x", Repos: []string{repo}, IdempotencyKey: "delivery-42"})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- task.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("expected one task, got %v", seen)
	}
	tasks, _ := e.Store.ListTasks()
	if len(tasks) != 1 {
		t.Fatalf("%d tasks on disk", len(tasks))
	}
}

// F: two workers claim the same task; exactly one executes.
func TestTwoWorkersOneExecutes(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low"}`)},
		"developer":  devStep("completed", "fixed", []string{"repoA/add.go", fixedAdd}),
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[],"checked":["x"]}`)},
	}}
	data := filepath.Join(tmp, "data")
	e1 := newTestEngine(t, sc, data)
	e2 := newTestEngine(t, sc, data) // second "process" on the same data dir
	task, _ := e1.CreateTask("Fix Add", nil, []string{repo}, "")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, e := range []*Engine{e1, e2} {
		wg.Add(1)
		go func(i int, e *Engine) {
			defer wg.Done()
			errs[i] = e.RunTask(context.Background(), task.ID)
		}(i, e)
	}
	wg.Wait()
	already := 0
	for _, err := range errs {
		if errors.Is(err, ErrAlreadyRunning) {
			already++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if already != 1 {
		t.Fatalf("expected exactly one ErrAlreadyRunning, got %v", errs)
	}
	runs, _ := e1.Store.Runs(task.ID)
	if len(runs) != 3 {
		t.Fatalf("expected 3 agent runs (one execution), got %d", len(runs))
	}
}

// G / CAS: a stale snapshot cannot overwrite a newer one.
func TestSaveTaskIsCompareAndSwap(t *testing.T) {
	st, _ := store.NewFileStore(t.TempDir())
	task := &domain.Task{ID: "task_cas", Goal: "x", Status: domain.StatusPending, CreatedAt: time.Now()}
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	a, _ := st.GetTask(task.ID)
	b, _ := st.GetTask(task.ID)
	a.Goal = "from A"
	if err := st.SaveTask(a); err != nil {
		t.Fatal(err)
	}
	b.Goal = "from B (stale)"
	if err := st.SaveTask(b); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale save must conflict, got %v", err)
	}
	cur, _ := st.GetTask(task.ID)
	if cur.Goal != "from A" || cur.Version != 2 {
		t.Fatalf("%+v", cur)
	}
	// Many concurrent incrementers: every successful save is serialised.
	var wg sync.WaitGroup
	ok := 0
	var mu sync.Mutex
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				x, _ := st.GetTask(task.ID)
				x.State.Steps++
				err := st.SaveTask(x)
				if err == nil {
					mu.Lock()
					ok++
					mu.Unlock()
					return
				}
				if !errors.Is(err, store.ErrConflict) {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	cur, _ = st.GetTask(task.ID)
	if ok != 20 || cur.State.Steps != 20 || cur.Version != 22 {
		t.Fatalf("ok=%d steps=%d version=%d", ok, cur.State.Steps, cur.Version)
	}
}

// Resolve racing the engine: a decision resolved while the task is no longer
// awaiting it (or twice) is rejected; the engine's own save wins.
func TestResolveDecisionTwiceConcurrently(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low","decision_request":{"importance":"high","question":"which?","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}]}}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTask("Fix Add", nil, []string{repo}, "")
	_ = e.RunTask(context.Background(), task.ID)
	ds, _ := e.Store.Decisions(task.ID)
	if len(ds) != 1 {
		t.Fatalf("%d decisions", len(ds))
	}
	var wg sync.WaitGroup
	results := make([]error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = e.ResolveDecision(task.ID, ds[0].ID, "a", "")
		}(i)
	}
	wg.Wait()
	okN := 0
	for _, err := range results {
		if err == nil {
			okN++
		}
	}
	if okN != 1 {
		t.Fatalf("exactly one resolve must succeed, got %d (%v)", okN, results)
	}
	got, _ := e.Store.GetTask(task.ID)
	if len(got.State.Notes) != 1 {
		t.Fatalf("guidance recorded %d times", len(got.State.Notes))
	}
}

// D: cancel while a test command with background children is running. The
// process group must die and the task must not be marked done.
func TestCancelDuringVerificationKillsChildrenAndLeavesTaskRecoverable(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	marker := filepath.Join(tmp, "alive")
	mk := "test:\n\t( while true; do date > " + marker + "; sleep 0.1; done ) &\n\tsleep 60\n"
	os.WriteFile(filepath.Join(repo, "Makefile"), []byte(mk), 0o644)
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "mk")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low"}`)},
		"developer":  devStep("completed", "fixed", []string{"repoA/add.go", fixedAdd}),
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	e.Cfg.RepeatRepro = 1
	task, _ := e.CreateTaskSpec(TaskSpec{Goal: "Fix Add", Repos: []string{repo}, TestCommand: "make test", Kind: domain.KindChange})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.RunTask(ctx, task.ID) }()
	// Wait until the hostile test is running (marker appears), then cancel.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("RunTask did not return after cancel")
	}
	time.Sleep(400 * time.Millisecond)
	st1, _ := os.Stat(marker)
	time.Sleep(400 * time.Millisecond)
	st2, _ := os.Stat(marker)
	if st1 != nil && st2 != nil && st2.ModTime().After(st1.ModTime()) {
		t.Fatal("background child survived cancellation")
	}
	got, _ := e.Store.GetTask(task.ID)
	if got.Status == domain.StatusDone {
		t.Fatal("cancelled task marked done")
	}
	// A restart classifies it as interrupted, never complete.
	e2 := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	if err := e2.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	got, _ = e2.Store.GetTask(task.ID)
	// The hostile command runs first as the baseline, so the interruption
	// lands in "understanding"; either way the task must be interrupted with
	// a resume point, and nothing may be recorded as a pass.
	if got.Status != domain.StatusInterrupted || got.State.ResumeStatus == "" {
		t.Fatalf("after restart: %s (resume %s)", got.Status, got.State.ResumeStatus)
	}
	arts, _ := e2.Store.Artifacts(task.ID)
	for _, a := range arts {
		if (a.Kind == domain.ArtTestRun || a.Kind == domain.ArtBaselineRun) && a.Passed != nil && *a.Passed {
			t.Fatal("cancelled run persisted as a pass")
		}
	}
}

// Packet versions are never duplicated when many readers rebuild at once
// (every GET rebuilds; two processes share the data dir).
func TestConcurrentPacketStateNoDuplicateVersions(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low"}`)},
		"developer":  devStep("completed", "fixed", []string{"repoA/add.go", fixedAdd}),
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[],"checked":["x"]}`)},
	}}
	data := filepath.Join(tmp, "data")
	e1 := newTestEngine(t, sc, data)
	e2 := newTestEngine(t, sc, data)
	task, _ := e1.CreateTask("Fix Add", nil, []string{repo}, "")
	if err := e1.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	// Change the source so the next rebuild yields a new version, then race.
	wt := filepath.Join(e1.WS.Root, task.ID, "repoA", "add.go")
	os.WriteFile(wt, []byte(fixedAdd+"\n// late\n"), 0o644)
	gitRun(t, filepath.Dir(wt), "commit", "-qam", "late")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		e := e1
		if i%2 == 1 {
			e = e2
		}
		go func() {
			defer wg.Done()
			if _, err := e.PacketState(task.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	all, _ := e1.Store.Packets(task.ID)
	seen := map[int]bool{}
	for _, p := range all {
		if seen[p.Version] {
			t.Fatalf("duplicate packet version %d", p.Version)
		}
		seen[p.Version] = true
	}
	if len(all) != 2 {
		t.Fatalf("expected exactly 2 versions, got %d", len(all))
	}
}
