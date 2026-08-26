// Package fakegh is a local GitHub API stand-in for tests.
package fakegh

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Package fakegh is a local stand-in for api.github.com used by tests.
//
// FakeGitHub is a local stand-in for api.github.com covering the calls
// Proofline makes. It counts every write so tests can assert at-most-once
// semantics, and can be switched to "revoked" (403) to simulate a removed
// installation.
// PRJSON mirrors the GitHub pull object shape Proofline parses.
type PRJSON struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Base    struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
}

type FakeGitHub struct {
	Server   *httptest.Server
	mu       sync.Mutex
	PRs      map[string]*PRJSON // "owner/repo#n"
	Statuses []map[string]any   // posted statuses
	Comments []string
	Revoked  bool
	Requests int
}

func NewFakeGitHub() *FakeGitHub {
	f := &FakeGitHub{PRs: map[string]*PRJSON{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *FakeGitHub) Close() { f.Server.Close() }

func (f *FakeGitHub) SetPR(owner, repo string, n int, base, head, title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pj := &PRJSON{Number: n, Title: title, HTMLURL: fmt.Sprintf("%s/%s/%s/pull/%d", f.Server.URL, owner, repo, n), State: "open"}
	pj.Base.SHA, pj.Base.Ref = base, "main"
	pj.Head.SHA, pj.Head.Ref = head, "feature"
	f.PRs[fmt.Sprintf("%s/%s#%d", owner, repo, n)] = pj
}

func (f *FakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests++
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		w.WriteHeader(401)
		return
	}
	if f.Revoked {
		w.WriteHeader(403)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// /repos/{owner}/{repo}/...
	if len(parts) < 4 || parts[0] != "repos" {
		w.WriteHeader(404)
		return
	}
	owner, repo := parts[1], parts[2]
	switch {
	case r.Method == "GET" && parts[3] == "pulls" && len(parts) == 5:
		pj, ok := f.PRs[fmt.Sprintf("%s/%s#%s", owner, repo, parts[4])]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(pj)
	case r.Method == "POST" && parts[3] == "statuses" && len(parts) == 5:
		var st map[string]any
		_ = json.NewDecoder(r.Body).Decode(&st)
		st["sha"] = parts[4]
		f.Statuses = append(f.Statuses, st)
		w.WriteHeader(201)
	case r.Method == "POST" && parts[3] == "issues" && len(parts) == 6 && parts[5] == "comments":
		var c map[string]string
		_ = json.NewDecoder(r.Body).Decode(&c)
		f.Comments = append(f.Comments, c["body"])
		w.WriteHeader(201)
	default:
		w.WriteHeader(404)
	}
}

// WebhookRequest builds a signed pull_request webhook.
func (f *FakeGitHub) WebhookRequest(secret, deliveryID, action, owner, repo string, n int, target string) *http.Request {
	f.mu.Lock()
	pj := f.PRs[fmt.Sprintf("%s/%s#%d", owner, repo, n)]
	f.mu.Unlock()
	payload := map[string]any{
		"action":       action,
		"repository":   map[string]any{"full_name": owner + "/" + repo},
		"pull_request": pj,
	}
	body, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req := httptest.NewRequest("POST", target, strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}
