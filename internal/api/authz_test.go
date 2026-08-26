package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orchestrator/internal/auth"
	"orchestrator/internal/domain"
	"orchestrator/internal/engine"
	"orchestrator/internal/executor"
	"orchestrator/internal/gitws"
	"orchestrator/internal/repos"
	"orchestrator/internal/router"
	"orchestrator/internal/store"
)

// P5 — cross-tenant attacks against every data route. Two workspaces (A, B),
// tokens for each role, and a task that belongs to A.

type harness struct {
	srv    *httptest.Server
	eng    *engine.Engine
	auth   *auth.Store
	wsA    string
	wsB    string
	tokens map[string]string // "A.owner", "A.viewer", "B.member", ...
	taskA  string
}

func mkRepo(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "m_test.go"), []byte("package m\nimport \"testing\"\nfunc TestOK(t *testing.T){}\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tmp := t.TempDir()
	data := filepath.Join(tmp, "data")
	st, _ := store.NewFileStore(data)
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: "```json\n{\"summary\":\"x\",\"uncertainty\":\"low\",\"decision_request\":{\"importance\":\"high\",\"question\":\"q?\",\"options\":[{\"id\":\"a\",\"label\":\"A\"}]}}\n```"},
	}}
	ws := gitws.NewManager(filepath.Join(data, "worktrees"))
	eng := engine.New(st, ws, map[string]executor.Executor{"mock": sc}, router.Rules{Executor: "mock", CheapModel: "m", StrongModel: "m"}, nil, engine.DefaultConfig())
	eng.Repos = repos.Open(data, eng.Policy)
	as, _ := auth.Open(data)
	wsA, _ := as.CreateWorkspace("A")
	wsB, _ := as.CreateWorkspace("B")
	h := &harness{eng: eng, auth: as, wsA: wsA.ID, wsB: wsB.ID, tokens: map[string]string{}}
	for _, r := range []auth.Role{auth.RoleOwner, auth.RoleAdmin, auth.RoleMember, auth.RoleReviewer, auth.RoleViewer} {
		for _, w := range []struct{ name, id string }{{"A", wsA.ID}, {"B", wsB.ID}} {
			u, _ := as.CreateUser(w.name + "-" + string(r))
			_ = as.SetMembership(u.ID, w.id, r)
			tok, _ := as.IssueToken(u.ID, "t")
			h.tokens[w.name+"."+string(r)] = tok
		}
	}
	// Repo + task in workspace A.
	repoDir := filepath.Join(tmp, "repoA")
	mkRepo(t, repoDir)
	rp, err := eng.Repos.Add(repoDir, wsA.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := eng.CreateTaskSpec(engine.TaskSpec{Goal: "x", Repos: []string{rp.ID}, WorkspaceID: wsA.ID, Kind: domain.KindChange})
	if err != nil {
		t.Fatal(err)
	}
	_ = eng.RunTask(context.Background(), task.ID) // pauses on the researcher's decision
	h.taskA = task.ID
	s := New(eng)
	s.Auth = as
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) do(t *testing.T, who, method, path string, body any) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, &buf)
	if tok, ok := h.tokens[who]; ok {
		req.Header.Set("Authorization", "Bearer "+tok)
	} else if who != "" {
		req.Header.Set("Authorization", "Bearer "+who) // raw/garbage token
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	b := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(b)
		sb.Write(b[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func TestUnauthenticatedAndGarbageTokens(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/tasks", "/tasks/" + h.taskA, "/tasks/" + h.taskA + "/packet", "/system", "/repos"} {
		if code, _ := h.do(t, "", "GET", path, nil); code != 401 {
			t.Errorf("no token GET %s → %d", path, code)
		}
		if code, _ := h.do(t, "plt_garbage", "GET", path, nil); code != 401 {
			t.Errorf("garbage token GET %s → %d", path, code)
		}
	}
	// The static UI carries no data and may load without a token.
	if code, _ := h.do(t, "", "GET", "/cases/"+h.taskA, nil); code != 200 {
		t.Errorf("ui → %d", code)
	}
}

func TestCrossTenantTaskRoutesAre404(t *testing.T) {
	h := newHarness(t)
	ds, _ := h.eng.Store.Decisions(h.taskA)
	routes := []struct{ method, path string }{
		{"GET", "/tasks/" + h.taskA},
		{"GET", "/tasks/" + h.taskA + "/events"},
		{"GET", "/tasks/" + h.taskA + "/decisions"},
		{"GET", "/tasks/" + h.taskA + "/packet"},
		{"GET", "/tasks/" + h.taskA + "/packet/versions/1"},
		{"GET", "/tasks/" + h.taskA + "/verdicts"},
		{"GET", "/tasks/" + h.taskA + "/effects"},
		{"POST", "/tasks/" + h.taskA + "/verdict"},
		{"POST", "/tasks/" + h.taskA + "/decisions/" + ds[0].ID + "/resolve"},
		{"POST", "/tasks/" + h.taskA + "/run"},
		{"POST", "/tasks/" + h.taskA + "/resume"},
		{"POST", "/tasks/" + h.taskA + "/github/post"},
	}
	for _, who := range []string{"B.owner", "B.admin", "B.member", "B.reviewer", "B.viewer"} {
		for _, rt := range routes {
			code, _ := h.do(t, who, rt.method, rt.path, map[string]any{"decision": "accept", "option": "a"})
			if code != 404 {
				t.Errorf("%s %s %s → %d, want 404", who, rt.method, rt.path, code)
			}
		}
	}
	// Listing from B never shows A's task.
	_, body := h.do(t, "B.owner", "GET", "/tasks", nil)
	if strings.Contains(body, h.taskA) {
		t.Fatal("A's task listed to B")
	}
	_, body = h.do(t, "A.viewer", "GET", "/tasks", nil)
	if !strings.Contains(body, h.taskA) {
		t.Fatal("A's task not listed to A")
	}
	_, body = h.do(t, "B.owner", "GET", "/repos", nil)
	if strings.Contains(body, "repoA") {
		t.Fatal("A's repository listed to B")
	}
}

func TestRoleMatrixWithinWorkspace(t *testing.T) {
	h := newHarness(t)
	ds, _ := h.eng.Store.Decisions(h.taskA)
	// Verdict: reviewer/admin/owner yes; member/viewer no. (The task is
	// paused on a decision — not executing — so a verdict on the current,
	// INSUFFICIENT packet is allowed and pinned to it.)
	for who, want := range map[string]int{"A.viewer": 403, "A.member": 403, "A.reviewer": 201, "A.admin": 201, "A.owner": 201} {
		if code, body := h.do(t, who, "POST", "/tasks/"+h.taskA+"/verdict", map[string]any{"decision": "accept"}); code != want {
			t.Errorf("verdict by %s → %d (%s), want %d", who, code, body, want)
		}
	}
	// Resolve: viewer/reviewer no.
	for _, who := range []string{"A.viewer", "A.reviewer"} {
		if code, _ := h.do(t, who, "POST", "/tasks/"+h.taskA+"/decisions/"+ds[0].ID+"/resolve", map[string]any{"option": "a"}); code != 403 {
			t.Errorf("resolve by %s → %d", who, code)
		}
	}
	// Create: viewer/reviewer no.
	for _, who := range []string{"A.viewer", "A.reviewer"} {
		if code, _ := h.do(t, who, "POST", "/tasks", map[string]any{"goal": "x", "repos": []string{"repo_x"}}); code != 403 {
			t.Errorf("create by %s → %d", who, code)
		}
	}
	// Repos: only admin/owner.
	for who, want := range map[string]int{"A.member": 403, "A.reviewer": 403, "A.viewer": 403} {
		if code, _ := h.do(t, who, "POST", "/repos", map[string]any{"path": "/nowhere"}); code != want {
			t.Errorf("add repo by %s → %d", who, code)
		}
	}
	// A member of B cannot create a task on A's repository even by ID.
	rps, _ := h.eng.Repos.List()
	if code, _ := h.do(t, "B.member", "POST", "/tasks", map[string]any{"goal": "steal", "repos": []string{rps[0].ID}}); code == 201 {
		t.Fatal("B created a task on A's repository")
	}
	// Member of A finally resolves; the same call by B stays 404.
	if code, body := h.do(t, "A.member", "POST", "/tasks/"+h.taskA+"/decisions/"+ds[0].ID+"/resolve", map[string]any{"option": "a"}); code != 200 {
		t.Fatalf("A.member resolve → %d %s", code, body)
	}
	// The resolve restarts the engine asynchronously; let it settle before
	// the temp dir is removed.
	for i := 0; i < 300; i++ {
		tk, _ := h.eng.Store.GetTask(h.taskA)
		if tk != nil && !tk.Status.Active() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestRevokedMembershipLosesAccessImmediately(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.do(t, "A.viewer", "GET", "/tasks/"+h.taskA, nil); code != 200 {
		t.Fatalf("viewer before revoke → %d", code)
	}
	p, _ := h.auth.Authenticate(h.tokens["A.viewer"])
	if err := h.auth.RevokeMembership(p.UserID, h.wsA); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.do(t, "A.viewer", "GET", "/tasks/"+h.taskA, nil); code != 404 {
		t.Fatalf("viewer after revoke → %d, want 404", code)
	}
	_, body := h.do(t, "A.viewer", "GET", "/tasks", nil)
	if strings.Contains(body, h.taskA) {
		t.Fatal("revoked member still lists the task")
	}
}

func TestWorkspaceHeaderCannotEscalate(t *testing.T) {
	h := newHarness(t)
	// A member of B claims workspace A in the header.
	req, _ := http.NewRequest("POST", h.srv.URL+"/tasks", strings.NewReader(`{"goal":"x","repos":["repo_x"]}`))
	req.Header.Set("Authorization", "Bearer "+h.tokens["B.member"])
	req.Header.Set("X-Workspace", h.wsA)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 403 {
		t.Fatalf("header escalation → %d", resp.StatusCode)
	}
}
