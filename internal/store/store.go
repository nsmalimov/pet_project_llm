// Package store persists tasks, events, decisions, evidence and agent runs.
// The interface is storage-agnostic; the shipped implementation is a plain
// file store (JSON snapshot + JSONL append logs) so the prototype has zero
// external dependencies and its state can be read with cat/jq. Swapping in
// SQLite later means implementing this interface, nothing else.
package store

import (
	"errors"

	"orchestrator/internal/domain"
)

// ErrConflict is returned by SaveTask when the snapshot changed underneath
// the caller (optimistic concurrency).
var ErrConflict = errors.New("task snapshot changed concurrently")

type Store interface {
	// LockTask takes an exclusive cross-process lock on the task (used by the
	// engine so a CLI and a server sharing a data dir cannot drive the same
	// task concurrently). Returns an unlock func, or an error if held.
	LockTask(id string) (func(), error)

	CreateTask(t *domain.Task) error
	// SaveTask is compare-and-swap: it fails with ErrConflict unless the
	// stored Version equals t.Version, then stores t with Version+1.
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

	AddArtifact(a domain.Artifact) error
	Artifacts(taskID string) ([]domain.Artifact, error)

	// Packets are append-only versions; Packets returns them in version order.
	// WithPacketLock serialises "read versions → append new version" across
	// goroutines and processes so version numbers are never duplicated.
	WithPacketLock(taskID string, fn func() error) error
	AddPacket(p domain.Packet) error
	Packets(taskID string) ([]domain.Packet, error)

	AddVerdict(v domain.Verdict) error
	Verdicts(taskID string) ([]domain.Verdict, error)

	// Audit trail (append-only, instance-wide).
	AddAudit(a domain.AuditRecord) error
	Audit(limit int) ([]domain.AuditRecord, error)

	// Idempotency: Claim returns (taskID, true) when key was already used.
	// Otherwise it records key→taskID atomically and returns (taskID, false).
	ClaimIdempotencyKey(key, taskID string) (string, bool, error)

	// External effects ledger (append-only; last record per key wins).
	// WithEffectsLock serialises check-then-append across processes.
	WithEffectsLock(taskID string, fn func() error) error
	AddEffect(e domain.ExternalEffect) error
	Effects(taskID string) ([]domain.ExternalEffect, error)

	CreateDecision(d *domain.Decision) error
	SaveDecision(d *domain.Decision) error
	GetDecision(taskID, id string) (*domain.Decision, error)
	Decisions(taskID string) ([]domain.Decision, error)
}
