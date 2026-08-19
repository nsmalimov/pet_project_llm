// Package store persists tasks, events, decisions, evidence and agent runs.
// The interface is storage-agnostic; the shipped implementation is a plain
// file store (JSON snapshot + JSONL append logs) so the prototype has zero
// external dependencies and its state can be read with cat/jq. Swapping in
// SQLite later means implementing this interface, nothing else.
package store

import "orchestrator/internal/domain"

type Store interface {
	// LockTask takes an exclusive cross-process lock on the task (used by the
	// engine so a CLI and a server sharing a data dir cannot drive the same
	// task concurrently). Returns an unlock func, or an error if held.
	LockTask(id string) (func(), error)

	CreateTask(t *domain.Task) error
	SaveTask(t *domain.Task) error
	GetTask(id string) (*domain.Task, error)
	ListTasks() ([]*domain.Task, error)

	// AppendEvent assigns Seq and At, persists, and returns the stored event.
	AppendEvent(taskID, typ string, data map[string]any) (domain.Event, error)
	// Events returns events with Seq > afterSeq, in order.
	Events(taskID string, afterSeq int64) ([]domain.Event, error)

	AddRun(r *domain.AgentRun) error
	UpdateRun(r *domain.AgentRun) error
	Runs(taskID string) ([]domain.AgentRun, error)

	AddEvidence(e domain.Evidence) error
	EvidenceList(taskID string) ([]domain.Evidence, error)

	CreateDecision(d *domain.Decision) error
	SaveDecision(d *domain.Decision) error
	GetDecision(taskID, id string) (*domain.Decision, error)
	Decisions(taskID string) ([]domain.Decision, error)
}
