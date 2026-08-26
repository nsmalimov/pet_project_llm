package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestTokenIssueRevokeAndWorkspaces(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(t, "A.owner", "POST", "/auth/tokens", map[string]any{"name": "laptop"})
	if code != 201 || !strings.Contains(body, "plt_") {
		t.Fatalf("issue → %d %s", code, body)
	}
	code, body = h.do(t, "A.owner", "GET", "/auth/tokens", nil)
	if code != 200 || strings.Contains(body, `"hash"`) || !strings.Contains(body, "laptop") {
		t.Fatalf("list → %d %s", code, body)
	}
	// Revoke my own token: subsequent calls with it fail.
	req, _ := http.NewRequest("GET", h.srv.URL+"/auth/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+h.tokens["A.viewer"])
	resp, _ := http.DefaultClient.Do(req)
	var toks []map[string]any
	decode(resp, &toks)
	tid := toks[0]["id"].(string)
	if code, _ := h.do(t, "A.owner", "DELETE", "/auth/tokens/"+tid, nil); code != 404 {
		t.Fatalf("revoking someone else's token → %d", code)
	}
	if code, _ := h.do(t, "A.viewer", "DELETE", "/auth/tokens/"+tid, nil); code != 200 {
		t.Fatalf("self revoke → %d", code)
	}
	if code, _ := h.do(t, "A.viewer", "GET", "/tasks", nil); code != 401 {
		t.Fatalf("revoked token still works → %d", code)
	}
	// Workspaces: members visible only to owners; B cannot add members to A.
	code, body = h.do(t, "A.owner", "GET", "/workspaces", nil)
	if code != 200 || !strings.Contains(body, `"members"`) || strings.Contains(body, h.wsB+`","name":"B"`) {
		t.Fatalf("workspaces → %d %s", code, body)
	}
	if code, _ := h.do(t, "B.owner", "POST", "/workspaces/"+h.wsA+"/members", map[string]any{"name": "x", "role": "member"}); code != 403 && code != 404 {
		t.Fatalf("cross-workspace add member → %d", code)
	}
	if code, _ := h.do(t, "A.admin", "POST", "/workspaces/"+h.wsA+"/members", map[string]any{"name": "x", "role": "member"}); code != 403 {
		t.Fatalf("admin add member → %d", code)
	}
	code, body = h.do(t, "A.owner", "POST", "/workspaces/"+h.wsA+"/members", map[string]any{"name": "newbie", "role": "reviewer"})
	if code != 201 || !strings.Contains(body, "plt_") {
		t.Fatalf("owner add member → %d %s", code, body)
	}
}

func decode(resp *http.Response, v any) {
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(v)
}
