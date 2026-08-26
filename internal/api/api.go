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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"orchestrator/examples"
	"orchestrator/fixtures"
	"orchestrator/internal/auth"
	"orchestrator/internal/domain"
	"orchestrator/internal/engine"
	"orchestrator/internal/github"
	"orchestrator/internal/repos"
	"orchestrator/internal/sandbox"
	"orchestrator/internal/store"
)

//go:embed ui.html
var uiHTML []byte

type Server struct {
	Engine *engine.Engine
	// Auth, when configured (at least one workspace), makes every data
	// route require a bearer token and resolves each resource to its
	// workspace before the handler runs. Unconfigured = local single-user
	// mode: a synthetic owner of the "local" workspace; serve refuses to
	// bind to a non-loopback address in that mode.
	Auth *auth.Store
	// GitHub is optional; without a token, webhook deliveries still import
	// (the payload carries the PR) but posting/refresh are refused.
	GitHub        *github.Client
	WebhookSecret string
	PublicURL     string // base URL for packet links in GitHub statuses
	// ExampleRoot, when set, enables Local Pilot examples: fixture repos are
	// materialised under it (must be outside the workspace root).
	ExampleRoot string
}

func New(e *engine.Engine) *Server { return &Server{Engine: e} }

// LocalWorkspace is the scope used when no auth is configured.
const LocalWorkspace = "local"

type ctxKey int

const principalKey ctxKey = 1

func principalOf(r *http.Request) *auth.Principal {
	p, _ := r.Context().Value(principalKey).(*auth.Principal)
	return p
}

// authenticate resolves the bearer token (or the local principal).
func (s *Server) authenticate(r *http.Request) (*auth.Principal, error) {
	if s.Auth == nil || !s.Auth.Configured() {
		return &auth.Principal{UserID: "local", Name: "local", Roles: map[string]auth.Role{LocalWorkspace: auth.RoleOwner}}, nil
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil, auth.ErrUnauthenticated
	}
	return s.Auth.Authenticate(strings.TrimPrefix(h, "Bearer "))
}

// protect wraps a handler: authenticate, then authorize `action` against
// the resource's workspace. Task routes resolve {id} → task → workspace and
// answer 404 for tasks outside the principal's workspaces so IDs cannot be
// enumerated. Workspace-level routes take the workspace from `ws`.
func (s *Server) protect(action auth.Action, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.authenticate(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalKey, p))
		if id := r.PathValue("id"); id != "" {
			t, err := s.Engine.Store.GetTask(id)
			if err != nil || !p.Can(auth.ActView, s.scopeOf(t)) {
				writeErr(w, store.ErrNotFound) // hide existence across tenants
				return
			}
			if !p.Can(action, s.scopeOf(t)) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: " + string(action)})
				return
			}
			h(w, r)
			return
		}
		if action == auth.ActView { // instance/list routes: any member
			h(w, r)
			return
		}
		ws := s.requestedWorkspace(r, p)
		if !p.Can(action, ws) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: " + string(action) + " in workspace " + ws})
			return
		}
		h(w, r)
	}
}

// scopeOf returns the workspace a task belongs to (legacy tasks: local).
func (s *Server) scopeOf(t *domain.Task) string {
	if t.WorkspaceID == "" {
		return LocalWorkspace
	}
	return t.WorkspaceID
}

// requestedWorkspace picks the workspace for a creation-style request: the
// X-Workspace header, else the principal's only workspace.
func (s *Server) requestedWorkspace(r *http.Request, p *auth.Principal) string {
	if ws := r.Header.Get("X-Workspace"); ws != "" {
		return ws
	}
	if wss := p.Workspaces(); len(wss) == 1 {
		return wss[0]
	}
	return ""
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", s.protect(auth.ActCreate, s.createTask))
	mux.HandleFunc("GET /tasks", s.protect(auth.ActView, s.listTasks))
	mux.HandleFunc("GET /tasks/{id}", s.protect(auth.ActView, s.getTask))
	mux.HandleFunc("GET /tasks/{id}/events", s.protect(auth.ActView, s.getEvents))
	mux.HandleFunc("GET /tasks/{id}/decisions", s.protect(auth.ActView, s.getDecisions))
	mux.HandleFunc("POST /tasks/{id}/decisions/{did}/resolve", s.protect(auth.ActResolve, s.resolveDecision))
	mux.HandleFunc("POST /tasks/{id}/resume", s.protect(auth.ActCreate, s.resumeTask))
	mux.HandleFunc("POST /tasks/{id}/run", s.protect(auth.ActCreate, s.runTask))
	mux.HandleFunc("GET /tasks/{id}/packet", s.protect(auth.ActView, s.getPacket))
	mux.HandleFunc("GET /tasks/{id}/packet/versions/{v}", s.protect(auth.ActView, s.getPacketVersion))
	mux.HandleFunc("POST /tasks/{id}/verdict", s.protect(auth.ActVerdict, s.postVerdict))
	mux.HandleFunc("GET /tasks/{id}/verdicts", s.protect(auth.ActView, s.getVerdicts))
	mux.HandleFunc("GET /tasks/{id}/effects", s.protect(auth.ActView, s.getEffects))
	mux.HandleFunc("POST /tasks/{id}/github/post", s.protect(auth.ActPostExternal, s.githubPost))
	mux.HandleFunc("GET /system", s.protect(auth.ActView, s.system))
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /examples", s.protect(auth.ActView, s.listExamples))
	mux.HandleFunc("POST /examples/{name}", s.protect(auth.ActCreate, s.runExample))
	mux.HandleFunc("POST /tasks/{id}/cancel", s.protect(auth.ActCreate, s.cancelTask))
	mux.HandleFunc("GET /repos", s.protect(auth.ActView, s.listRepos))
	mux.HandleFunc("POST /repos", s.protect(auth.ActManageRepos, s.addRepo))
	mux.HandleFunc("POST /github/import", s.protect(auth.ActCreate, s.githubImport))
	// Webhook: authenticated by HMAC signature, scoped by the repository link.
	mux.HandleFunc("POST /github/webhook", s.githubWebhook)
	// UI (static, embedded; carries no data).
	mux.HandleFunc("GET /", s.ui)
	mux.HandleFunc("GET /cases/{id}", s.ui)
	mux.HandleFunc("GET /cases/{id}/{tab}", s.ui)
	mux.HandleFunc("GET /new", s.ui)
	mux.HandleFunc("GET /repos-ui", s.ui)
	mux.HandleFunc("GET /help", s.ui)
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
	Kind         string   `json:"kind,omitempty"`     // bugfix | change (inferred when empty)
	HeadRef      string   `json:"head_ref,omitempty"` // verify-only mode: existing change to verify
	// Start controls whether execution begins immediately (default true).
	Start *bool `json:"start,omitempty"`
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	p := principalOf(r)
	ws := s.requestedWorkspace(r, p)
	t, existing, err := s.Engine.CreateTaskIdempotent(engine.TaskSpec{
		Goal: req.Goal, Context: req.Context, Repos: req.Repos,
		TestCommand: req.TestCommand, ReproCommand: req.ReproCommand, Kind: domain.TaskKind(req.Kind), HeadRef: req.HeadRef,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), WorkspaceID: ws,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if existing {
		if !p.Can(auth.ActView, s.scopeOf(t)) {
			writeErr(w, store.ErrNotFound)
			return
		}
		writeJSON(w, http.StatusOK, t) // replayed: no second run
		return
	}
	if req.Start == nil || *req.Start {
		s.runAsync(t.ID)
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) runAsync(id string) {
	go func() {
		err := s.Engine.RunTask(context.Background(), id)
		if errors.Is(err, context.Canceled) {
			s.Engine.MarkInterrupted(id, "cancelled by user")
			return
		}
		if err != nil && !errors.Is(err, engine.ErrAlreadyRunning) {
			log.Printf("task %s: %v", id, err)
		}
	}()
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.Engine.Cancel(id) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "task is not running in this process"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"task": id, "status": "cancelling"})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "exec_mode": s.Engine.Policy.Mode})
}

// ---------- Local Pilot examples ----------

func (s *Server) listExamples(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"available": s.ExampleRoot != "",
		"note":      "Local Pilot examples run the REAL engine (worktree, baseline, go test, original-test replay, packet) on the embedded reservations fixture; only the agents' replies are scripted.",
		"scenarios": examples.List(),
	})
}

// runExample materialises a fresh fixture repository (outside the workspace
// root, under ExampleRoot), registers it and starts a scenario case.
func (s *Server) runExample(w http.ResponseWriter, r *http.Request) {
	if s.ExampleRoot == "" || s.Engine.Repos == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Local Pilot examples are not enabled on this instance"})
		return
	}
	name := r.PathValue("name")
	_, meta, err := examples.Load(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	dir := filepath.Join(s.ExampleRoot, name+"-"+domain.NewID("fx")[3:], "reservations")
	if err := fixtures.Materialize(dir); err != nil {
		writeErr(w, err)
		return
	}
	p := principalOf(r)
	ws := s.requestedWorkspace(r, p)
	if ws == LocalWorkspace {
		ws = ""
	}
	rp, err := s.Engine.Repos.Add(dir, ws)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	t, err := s.Engine.CreateTaskSpec(engine.TaskSpec{
		Goal: examples.Goal(meta), Repos: []string{rp.ID}, ReproCommand: examples.ReproCommand,
		Kind: domain.KindBugfix, WorkspaceID: s.requestedWorkspace(r, p), Scenario: name,
		Context: examples.Context(meta),
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.runAsync(t.ID)
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	all, err := s.Engine.Store.ListTasks()
	if err != nil {
		writeErr(w, err)
		return
	}
	p := principalOf(r)
	ts := []*domain.Task{}
	for _, t := range all {
		if p.Can(auth.ActView, s.scopeOf(t)) {
			ts = append(ts, t)
		}
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
	// PacketVersion is the version the human looked at; a mismatch is 409.
	PacketVersion int `json:"packet_version,omitempty"`
}

func (s *Server) postVerdict(w http.ResponseWriter, r *http.Request) {
	var req verdictReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	v, err := s.Engine.RecordVerdict(r.PathValue("id"), req.Decision, req.Note, req.By, req.PacketVersion)
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrTaskRunning), errors.Is(err, engine.ErrPacketChanged):
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
	switch {
	case r.URL.Path == "/", r.URL.Path == "/new", r.URL.Path == "/repos-ui", r.URL.Path == "/help", strings.HasPrefix(r.URL.Path, "/cases/"):
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(uiHTML)
}

// system reports the execution boundary so no client can assume safety.
func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	p := s.Engine.Policy
	execs := []string{}
	for name := range s.Engine.Execs {
		execs = append(execs, name)
	}
	sort.Strings(execs)
	writeJSON(w, http.StatusOK, map[string]any{
		"exec_mode": p.Mode, "warning": p.Warning(), "capabilities": p.Capabilities(),
		"workspace_root": p.WorkspaceRoot, "repos_roots": p.ReposRoots,
		"github_connected": s.GitHub != nil && s.GitHub.Token != "", "webhook_configured": s.WebhookSecret != "",
		"executors": execs, "examples_enabled": s.ExampleRoot != "",
		"auth_configured":    s.Auth != nil && s.Auth.Configured(),
		"integration_runner": "not configured",
	})
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) {
	if s.Engine.Repos == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	all, err := s.Engine.Repos.List()
	if err != nil {
		writeErr(w, err)
		return
	}
	p := principalOf(r)
	list := []repos.Repo{}
	for _, rp := range all {
		ws := rp.Workspace
		if ws == "" {
			ws = LocalWorkspace
		}
		if p.Can(auth.ActView, ws) {
			list = append(list, rp)
		}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) addRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || s.Engine.Repos == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	ws := s.requestedWorkspace(r, principalOf(r))
	if ws == LocalWorkspace {
		ws = ""
	}
	rp, err := s.Engine.Repos.Add(req.Path, ws)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, rp)
}

// ---------- GitHub ----------

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	d, err := github.ParseDelivery(s.WebhookSecret, r)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, github.ErrBadSignature) {
			code = http.StatusUnauthorized
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	if s.Engine.Policy.Mode != sandbox.ModeSafe && os.Getenv("PROOFLINE_ALLOW_UNSAFE_WEBHOOKS") == "" {
		// A webhook runs code from whoever opened the PR. Without an OS
		// sandbox that is host code execution for strangers.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "BLOCKED: webhook imports are refused in LOCAL_UNSAFE (run SAFE_SANDBOX, or set PROOFLINE_ALLOW_UNSAFE_WEBHOOKS=1 for trusted repositories only)", "delivery": d.ID})
		return
	}
	t, existing, err := s.Engine.HandleDelivery(r.Context(), d)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "delivery": d.ID})
		return
	}
	if t == nil {
		writeJSON(w, http.StatusOK, map[string]string{"ignored": d.Event + "/" + d.Action, "delivery": d.ID})
		return
	}
	if !existing {
		s.runAsync(t.ID)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"task": t.ID, "existing": existing, "head_sha": t.PR.HeadSHA, "delivery": d.ID})
}

func (s *Server) githubImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
		Number int    `json:"number"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Owner == "" || req.Repo == "" || req.Number <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "owner, repo, number required"})
		return
	}
	if s.GitHub == nil || s.GitHub.Token == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "BLOCKED: no GitHub token configured"})
		return
	}
	t, err := s.Engine.RefreshPR(r.Context(), s.GitHub, req.Owner, req.Repo, req.Number)
	if err == nil && !principalOf(r).Can(auth.ActCreate, s.scopeOf(t)) {
		writeErr(w, store.ErrNotFound)
		return
	}
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, github.ErrRevoked) {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	if !t.Status.Terminal() && t.Status == domain.StatusPending {
		s.runAsync(t.ID)
	}
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) githubPost(w http.ResponseWriter, r *http.Request) {
	if s.GitHub == nil || s.GitHub.Token == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "BLOCKED: no GitHub token configured"})
		return
	}
	id := r.PathValue("id")
	effs, err := s.Engine.PostGitHubStatus(r.Context(), s.GitHub, id, s.PublicURL+"/cases/"+id)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, engine.ErrEffectUnknown) {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]any{"error": err.Error(), "effects": effs})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"effects": effs})
}

func (s *Server) getEffects(w http.ResponseWriter, r *http.Request) {
	effs, err := s.Engine.Store.Effects(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if effs == nil {
		effs = []domain.ExternalEffect{}
	}
	writeJSON(w, http.StatusOK, effs)
}
