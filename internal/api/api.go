// Package api exposes the orchestrator over a minimal HTTP JSON API.
// No auth, no versioning — a local prototype surface for a future UI.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"orchestrator/internal/domain"
	"orchestrator/internal/engine"
	"orchestrator/internal/store"
)

//go:embed ui.html
var uiHTML []byte

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
	// Proofline: change case packet + human verdict.
	mux.HandleFunc("GET /tasks/{id}/packet", s.getPacket)
	mux.HandleFunc("GET /tasks/{id}/packet/versions/{v}", s.getPacketVersion)
	mux.HandleFunc("POST /tasks/{id}/verdict", s.postVerdict)
	mux.HandleFunc("GET /tasks/{id}/verdicts", s.getVerdicts)
	// UI (static, embedded). Every screen loads its data from the API above.
	mux.HandleFunc("GET /", s.ui)
	mux.HandleFunc("GET /cases/{id}", s.ui)
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
	Repos        []string `json:"repos"`
	Goal         string   `json:"goal"`
	Context      []string `json:"context,omitempty"`
	TestCommand  string   `json:"test_command,omitempty"`
	ReproCommand string   `json:"repro_command,omitempty"`
	Kind         string   `json:"kind,omitempty"` // bugfix | change (inferred when empty)
	// Start controls whether execution begins immediately (default true).
	Start *bool `json:"start,omitempty"`
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	t, err := s.Engine.CreateTaskSpec(engine.TaskSpec{
		Goal: req.Goal, Context: req.Context, Repos: req.Repos,
		TestCommand: req.TestCommand, ReproCommand: req.ReproCommand, Kind: domain.TaskKind(req.Kind),
	})
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

// ---------- Proofline ----------

func (s *Server) getPacket(w http.ResponseWriter, r *http.Request) {
	v, err := s.Engine.PacketState(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) getPacketVersion(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("v"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad version"})
		return
	}
	p, err := s.Engine.PacketVersion(r.PathValue("id"), n)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type verdictReq struct {
	Decision string `json:"decision"` // accept | request_changes | reject
	Note     string `json:"note,omitempty"`
	By       string `json:"by,omitempty"`
}

func (s *Server) postVerdict(w http.ResponseWriter, r *http.Request) {
	var req verdictReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	v, err := s.Engine.RecordVerdict(r.PathValue("id"), req.Decision, req.Note, req.By)
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrTaskRunning):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, err)
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) getVerdicts(w http.ResponseWriter, r *http.Request) {
	vs, err := s.Engine.Store.Verdicts(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if vs == nil {
		vs = []domain.Verdict{}
	}
	writeJSON(w, http.StatusOK, vs)
}

func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/cases/") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(uiHTML)
}
