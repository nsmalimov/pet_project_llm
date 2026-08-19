package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orchestrator/internal/domain"
)

func newStore(t *testing.T) *FileStore {
	t.Helper()
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTaskRoundtrip(t *testing.T) {
	s := newStore(t)
	task := &domain.Task{ID: "task_1", Goal: "g", Status: domain.StatusPending, CreatedAt: time.Now()}
	if err := s.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	task.Status = domain.StatusImplementing
	task.State.Notes = append(task.State.Notes, "note")
	if err := s.SaveTask(task); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask("task_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusImplementing || len(got.State.Notes) != 1 {
		t.Fatalf("roundtrip lost data: %+v", got)
	}
	if _, err := s.GetTask("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestEventSeqMonotonicAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	task := &domain.Task{ID: "task_1", Status: domain.StatusPending}
	if err := s.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.AppendEvent("task_1", "x", nil); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate restart: a fresh store must continue the sequence.
	s2, _ := NewFileStore(dir)
	ev, err := s2.AppendEvent("task_1", "y", map[string]any{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 4 {
		t.Fatalf("seq=%d, want 4", ev.Seq)
	}
	evs, _ := s2.Events("task_1", 2)
	if len(evs) != 2 || evs[0].Seq != 3 || evs[1].Seq != 4 {
		t.Fatalf("after-filter wrong: %+v", evs)
	}
}

func TestRunsDedupKeepsLast(t *testing.T) {
	s := newStore(t)
	task := &domain.Task{ID: "task_1", Status: domain.StatusPending}
	if err := s.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	r := &domain.AgentRun{ID: "run_1", TaskID: "task_1", Status: "running"}
	if err := s.AddRun(r); err != nil {
		t.Fatal(err)
	}
	r.Status = "ok"
	r.CostUSD = 0.5
	if err := s.UpdateRun(r); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs("task_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != "ok" || runs[0].CostUSD != 0.5 {
		t.Fatalf("dedup wrong: %+v", runs)
	}
}

func TestDecisionLifecycle(t *testing.T) {
	s := newStore(t)
	task := &domain.Task{ID: "task_1", Status: domain.StatusPending}
	if err := s.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	d := &domain.Decision{ID: "dec_1", TaskID: "task_1", Question: "q", Status: "open"}
	if err := s.CreateDecision(d); err != nil {
		t.Fatal(err)
	}
	d.Status = "resolved"
	d.ChosenOption = "a"
	if err := s.SaveDecision(d); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDecision("task_1", "dec_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "resolved" || got.ChosenOption != "a" {
		t.Fatalf("wrong: %+v", got)
	}
}

func TestLockTaskExclusive(t *testing.T) {
	s := newStore(t)
	task := &domain.Task{ID: "task_1", Status: domain.StatusPending}
	if err := s.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	unlock, err := s.LockTask("task_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LockTask("task_1"); err == nil {
		t.Fatal("second lock must fail while held")
	}
	unlock()
	unlock2, err := s.LockTask("task_1")
	if err != nil {
		t.Fatalf("lock after unlock failed: %v", err)
	}
	unlock2()
}

func TestTornLastEventLineIsTolerated(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	task := &domain.Task{ID: "task_1", Status: domain.StatusPending}
	if err := s.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent("task_1", "x", nil); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-append: torn partial JSON on the last line.
	f, _ := os.OpenFile(filepath.Join(dir, "tasks", "task_1", "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"seq":2,"task_id":"task_1","ty`)
	f.Close()

	s2, _ := NewFileStore(dir)
	evs, err := s2.Events("task_1", 0)
	if err != nil {
		t.Fatalf("torn last line must not poison reads: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	// Appending must continue the sequence past the torn line.
	ev, err := s2.AppendEvent("task_1", "y", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 2 {
		t.Fatalf("seq=%d, want 2", ev.Seq)
	}
}
