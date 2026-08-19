// Package api exposes the orchestrator over a minimal HTTP JSON API.
// No auth, no versioning — a local prototype surface for a future UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"orchestrator/internal/domain"
	"orchestrator/internal/engine"
	"orchestrator/internal/store"
)

type Server struct {
	Engine *engine.Engine
}

func New(e *engine.Engine) *Server { return &Server{Engine: e} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", s.createTask)
	mux.HandleFunc("GET /tasks", s.listTasks)
	mux.HandleFunc("GET /tasks/{id}", s.getTask)
	mux.HandleFunc("GET /tasks/{id}/events", s.getEvents)
	mux.HandleFunc("GET /tasks/{id}/decisions", s.getDecisions)
	mux.HandleFunc("POST /tasks/{id}/decisions/{did}/resolve", s.resolveDecision)
	mux.HandleFunc("POST /tasks/{id}/resume", s.resumeTask)
	mux.HandleFunc("POST /tasks/{id}/run", s.runTask)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if errors.Is(err, store.ErrNotFound) {
		code = http.StatusNotFound
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

type createTaskReq struct {
	Repos       []string `json:"repos"`
	Goal        string   `json:"goal"`
	Context     []string `json:"context,omitempty"`
	TestCommand string   `json:"test_command,omitempty"`
	// Start controls whether execution begins immediately (default true).
	Start *bool `json:"start,omitempty"`
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	t, err := s.Engine.CreateTask(req.Goal, req.Context, req.Repos, req.TestCommand)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Start == nil || *req.Start {
		s.runAsync(t.ID)
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) runAsync(id string) {
	go func() {
		if err := s.Engine.RunTask(context.Background(), id); err != nil && !errors.Is(err, engine.ErrAlreadyRunning) {
			log.Printf("task %s: %v", id, err)
		}
	}()
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Engine.Store.ListTasks()
	if err != nil {
		writeErr(w, err)
		return
	}
	if ts == nil {
		ts = []*domain.Task{}
	}
	writeJSON(w, http.StatusOK, ts)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	fs, err := s.Engine.FullState(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fs)
}

func (s *Server) getEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	evs, err := s.Engine.Store.Events(r.PathValue("id"), after)
	if err != nil {
		writeErr(w, err)
		return
	}
	if evs == nil {
		evs = []domain.Event{}
	}
	writeJSON(w, http.StatusOK, evs)
}

func (s *Server) getDecisions(w http.ResponseWriter, r *http.Request) {
	ds, err := s.Engine.Store.Decisions(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if ds == nil {
		ds = []domain.Decision{}
	}
	writeJSON(w, http.StatusOK, ds)
}

type resolveReq struct {
	Option string `json:"option"`
	Note   string `json:"note,omitempty"`
}

func (s *Server) resolveDecision(w http.ResponseWriter, r *http.Request) {
	var req resolveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	t, err := s.Engine.ResolveDecision(r.PathValue("id"), r.PathValue("did"), req.Option, req.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !t.Status.Terminal() {
		s.runAsync(t.ID)
	}
	writeJSON(w, http.StatusOK, t)
}

// runTask starts execution of a task that is not currently running — e.g.
// one created with start:false, or one whose engine loop stopped without
// reaching a terminal state.
func (s *Server) runTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.Engine.Store.GetTask(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	switch {
	case t.Status.Terminal():
		writeJSON(w, http.StatusConflict, map[string]string{"error": "task is " + string(t.Status)})
		return
	case t.Status == domain.StatusAwaitingDecision:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "task awaits a decision; resolve it first"})
		return
	case t.Status == domain.StatusInterrupted:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "task is interrupted; use /resume"})
		return
	}
	s.runAsync(t.ID)
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) resumeTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.Engine.Resume(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.runAsync(t.ID)
	writeJSON(w, http.StatusOK, t)
}
