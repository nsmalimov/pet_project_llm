package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"orchestrator/internal/domain"
)

// ErrNotFound is returned when a task or decision does not exist.
var ErrNotFound = errors.New("not found")

// FileStore keeps every task in its own directory:
//
//	<root>/tasks/<id>/task.json      — snapshot of the task
//	<root>/tasks/<id>/events.jsonl   — append-only event log
//	<root>/tasks/<id>/runs.jsonl     — agent runs (updates appended, last wins)
//	<root>/tasks/<id>/evidence.jsonl — evidence records
//	<root>/tasks/<id>/decisions.json — all decisions of the task
type FileStore struct {
	root string

	mu   sync.Mutex // guards locks map
	lock map[string]*taskLock
}

type taskLock struct {
	mu      sync.Mutex
	pmu     sync.Mutex // packet version serialisation
	lastSeq int64      // 0 = unknown, recomputed lazily from events.jsonl
}

func NewFileStore(root string) (*FileStore, error) {
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o755); err != nil {
		return nil, err
	}
	return &FileStore{root: root, lock: map[string]*taskLock{}}, nil
}

func (s *FileStore) taskDir(id string) string { return filepath.Join(s.root, "tasks", id) }

func (s *FileStore) lockFor(id string) *taskLock {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.lock[id]
	if !ok {
		l = &taskLock{}
		s.lock[id] = l
	}
	return l
}

// writeJSON writes v to path atomically (tmp file + rename).
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func appendJSONL(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// readJSONL decodes every line of path into T. A missing file yields nil.
// A torn final line (crash mid-append) is tolerated and skipped; corruption
// anywhere else is an error.
func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var pendingErr error
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if pendingErr != nil {
			// A bad line followed by more data is real corruption.
			return out, pendingErr
		}
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			pendingErr = fmt.Errorf("decode %s: %w", path, err)
			continue
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil // pendingErr on the last line = torn write, ignore
}

// LockTask takes an exclusive flock on <task-dir>/.lock so only one process
// drives a task at a time (a CLI and a server may share a data dir).
func (s *FileStore) LockTask(id string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.taskDir(id), ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("task %s is locked by another process", id)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// ---------- tasks ----------

func (s *FileStore) CreateTask(t *domain.Task) error {
	dir := s.taskDir(t.ID)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("task %s already exists", t.ID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	t.Version = 1
	return writeJSON(filepath.Join(dir, "task.json"), t)
}

// SaveTask is compare-and-swap across goroutines (mutex) and processes
// (blocking flock on <task>/.cas): read stored version, compare, write.
func (s *FileStore) SaveTask(t *domain.Task) error {
	l := s.lockFor(t.ID)
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := s.fileLock(filepath.Join(s.taskDir(t.ID), ".cas"))
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := s.GetTask(t.ID)
	if err != nil {
		return err
	}
	if cur.Version != t.Version {
		return fmt.Errorf("%w: task %s stored v%d, caller has v%d", ErrConflict, t.ID, cur.Version, t.Version)
	}
	t.Version++
	t.UpdatedAt = time.Now().UTC()
	if err := writeJSON(filepath.Join(s.taskDir(t.ID), "task.json"), t); err != nil {
		t.Version-- // keep the caller's view consistent with disk
		return err
	}
	return nil
}

// fileLock takes a blocking exclusive flock on path.
func (s *FileStore) fileLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// ---------- idempotency & external effects ----------

func (s *FileStore) ClaimIdempotencyKey(key, taskID string) (string, bool, error) {
	path := filepath.Join(s.root, "idempotency.json")
	unlock, err := s.fileLock(path + ".lock")
	if err != nil {
		return "", false, err
	}
	defer unlock()
	m := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	if existing, ok := m[key]; ok {
		return existing, true, nil
	}
	m[key] = taskID
	return taskID, false, writeJSON(path, m)
}

func (s *FileStore) AddEffect(e domain.ExternalEffect) error {
	return appendJSONL(filepath.Join(s.taskDir(e.TaskID), "effects.jsonl"), e)
}

func (s *FileStore) Effects(taskID string) ([]domain.ExternalEffect, error) {
	return readJSONL[domain.ExternalEffect](filepath.Join(s.taskDir(taskID), "effects.jsonl"))
}

func (s *FileStore) GetTask(id string) (*domain.Task, error) {
	b, err := os.ReadFile(filepath.Join(s.taskDir(id), "task.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("task %s: %w", id, ErrNotFound)
		}
		return nil, err
	}
	var t domain.Task
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *FileStore) ListTasks() ([]*domain.Task, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "tasks"))
	if err != nil {
		return nil, err
	}
	var out []*domain.Task
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := s.GetTask(e.Name())
		if err != nil {
			continue // skip corrupt/partial dirs, don't fail the listing
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ---------- events ----------

func (s *FileStore) AppendEvent(taskID, typ string, data map[string]any) (domain.Event, error) {
	l := s.lockFor(taskID)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastSeq == 0 {
		evs, err := readJSONL[domain.Event](filepath.Join(s.taskDir(taskID), "events.jsonl"))
		if err != nil {
			return domain.Event{}, err
		}
		for _, e := range evs {
			if e.Seq > l.lastSeq {
				l.lastSeq = e.Seq
			}
		}
	}
	l.lastSeq++
	ev := domain.Event{Seq: l.lastSeq, TaskID: taskID, Type: typ, At: time.Now().UTC(), Data: data}
	if err := appendJSONL(filepath.Join(s.taskDir(taskID), "events.jsonl"), ev); err != nil {
		return domain.Event{}, err
	}
	return ev, nil
}

func (s *FileStore) Events(taskID string, afterSeq int64) ([]domain.Event, error) {
	evs, err := readJSONL[domain.Event](filepath.Join(s.taskDir(taskID), "events.jsonl"))
	if err != nil {
		return nil, err
	}
	if afterSeq <= 0 {
		return evs, nil
	}
	var out []domain.Event
	for _, e := range evs {
		if e.Seq > afterSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

// ---------- agent runs ----------

func (s *FileStore) AddRun(r *domain.AgentRun) error {
	return appendJSONL(filepath.Join(s.taskDir(r.TaskID), "runs.jsonl"), r)
}

// UpdateRun appends the updated record; Runs() deduplicates keeping the last.
func (s *FileStore) UpdateRun(r *domain.AgentRun) error { return s.AddRun(r) }

func (s *FileStore) Runs(taskID string) ([]domain.AgentRun, error) {
	all, err := readJSONL[domain.AgentRun](filepath.Join(s.taskDir(taskID), "runs.jsonl"))
	if err != nil {
		return nil, err
	}
	byID := map[string]int{}
	var out []domain.AgentRun
	for _, r := range all {
		if i, ok := byID[r.ID]; ok {
			out[i] = r
			continue
		}
		byID[r.ID] = len(out)
		out = append(out, r)
	}
	return out, nil
}

// ---------- evidence ----------

func (s *FileStore) AddEvidence(e domain.Evidence) error {
	return appendJSONL(filepath.Join(s.taskDir(e.TaskID), "evidence.jsonl"), e)
}

func (s *FileStore) EvidenceList(taskID string) ([]domain.Evidence, error) {
	return readJSONL[domain.Evidence](filepath.Join(s.taskDir(taskID), "evidence.jsonl"))
}

// ---------- decisions ----------

func (s *FileStore) AddArtifact(a domain.Artifact) error {
	return appendJSONL(filepath.Join(s.taskDir(a.TaskID), "artifacts.jsonl"), a)
}

func (s *FileStore) Artifacts(taskID string) ([]domain.Artifact, error) {
	return readJSONL[domain.Artifact](filepath.Join(s.taskDir(taskID), "artifacts.jsonl"))
}

func (s *FileStore) WithPacketLock(taskID string, fn func() error) error {
	l := s.lockFor(taskID)
	l.pmu.Lock()
	defer l.pmu.Unlock()
	unlock, err := s.fileLock(filepath.Join(s.taskDir(taskID), ".packets"))
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

func (s *FileStore) AddPacket(p domain.Packet) error {
	return appendJSONL(filepath.Join(s.taskDir(p.TaskID), "packets.jsonl"), p)
}

func (s *FileStore) Packets(taskID string) ([]domain.Packet, error) {
	return readJSONL[domain.Packet](filepath.Join(s.taskDir(taskID), "packets.jsonl"))
}

func (s *FileStore) AddVerdict(v domain.Verdict) error {
	return appendJSONL(filepath.Join(s.taskDir(v.TaskID), "verdicts.jsonl"), v)
}

func (s *FileStore) Verdicts(taskID string) ([]domain.Verdict, error) {
	return readJSONL[domain.Verdict](filepath.Join(s.taskDir(taskID), "verdicts.jsonl"))
}

func (s *FileStore) decisionsPath(taskID string) string {
	return filepath.Join(s.taskDir(taskID), "decisions.json")
}

func (s *FileStore) readDecisions(taskID string) ([]domain.Decision, error) {
	b, err := os.ReadFile(s.decisionsPath(taskID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []domain.Decision
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *FileStore) CreateDecision(d *domain.Decision) error {
	l := s.lockFor(d.TaskID)
	l.mu.Lock()
	defer l.mu.Unlock()
	ds, err := s.readDecisions(d.TaskID)
	if err != nil {
		return err
	}
	ds = append(ds, *d)
	return writeJSON(s.decisionsPath(d.TaskID), ds)
}

func (s *FileStore) SaveDecision(d *domain.Decision) error {
	l := s.lockFor(d.TaskID)
	l.mu.Lock()
	defer l.mu.Unlock()
	ds, err := s.readDecisions(d.TaskID)
	if err != nil {
		return err
	}
	for i := range ds {
		if ds[i].ID == d.ID {
			ds[i] = *d
			return writeJSON(s.decisionsPath(d.TaskID), ds)
		}
	}
	return fmt.Errorf("decision %s: %w", d.ID, ErrNotFound)
}

func (s *FileStore) GetDecision(taskID, id string) (*domain.Decision, error) {
	ds, err := s.readDecisions(taskID)
	if err != nil {
		return nil, err
	}
	for i := range ds {
		if ds[i].ID == id {
			return &ds[i], nil
		}
	}
	return nil, fmt.Errorf("decision %s: %w", id, ErrNotFound)
}

func (s *FileStore) Decisions(taskID string) ([]domain.Decision, error) {
	return s.readDecisions(taskID)
}
