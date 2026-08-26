package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// PullRequest is the subset of the GitHub PR object Proofline binds to.
type PullRequest struct {
	Owner   string
	Repo    string
	Number  int
	Title   string
	Body    string
	URL     string
	BaseSHA string
	BaseRef string
	HeadSHA string
	HeadRef string
	State   string // open | closed
}

// ErrRevoked marks a permission/installation problem: the integration can
// no longer see the repository. Callers must surface BLOCKED, not retry.
var ErrRevoked = errors.New("github: access revoked or repository not visible")

type prJSON struct {
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

// FetchPR reads a pull request. 401/403/404 → ErrRevoked (the repository
// may have been deleted, the installation removed or the token narrowed).
func (c *Client) FetchPR(owner, repo string, number int) (*PullRequest, error) {
	if c.Token == "" {
		return nil, fmt.Errorf("github: no token (set GITHUB_TOKEN)")
	}
	base := c.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/repos/%s/%s/pulls/%d", base, owner, repo, number), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 401, 403, 404:
		return nil, fmt.Errorf("%w (%d)", ErrRevoked, resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github: GET pull %d → %d", number, resp.StatusCode)
	}
	var pj prJSON
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pj); err != nil {
		return nil, err
	}
	return &PullRequest{Owner: owner, Repo: repo, Number: pj.Number, Title: pj.Title, Body: pj.Body, URL: pj.HTMLURL,
		BaseSHA: pj.Base.SHA, BaseRef: pj.Base.Ref, HeadSHA: pj.Head.SHA, HeadRef: pj.Head.Ref, State: pj.State}, nil
}

// ---------- webhooks ----------

// Delivery is the parsed, signature-verified content of one webhook.
type Delivery struct {
	ID     string // X-GitHub-Delivery — the idempotency key
	Event  string // X-GitHub-Event
	Action string
	PR     *PullRequest
	Repo   string // owner/name
}

var ErrBadSignature = errors.New("github: webhook signature invalid or missing")

// VerifySignature checks X-Hub-Signature-256 against the shared secret.
func VerifySignature(secret string, body []byte, header string) error {
	if secret == "" {
		return errors.New("github: webhook secret not configured")
	}
	if !strings.HasPrefix(header, "sha256=") {
		return ErrBadSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(strings.TrimPrefix(header, "sha256="))) {
		return ErrBadSignature
	}
	return nil
}

// ParseDelivery verifies and parses a webhook request. Body size is capped.
func ParseDelivery(secret string, r *http.Request) (*Delivery, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if err := VerifySignature(secret, body, r.Header.Get("X-Hub-Signature-256")); err != nil {
		return nil, err
	}
	d := &Delivery{ID: r.Header.Get("X-GitHub-Delivery"), Event: r.Header.Get("X-GitHub-Event")}
	if d.ID == "" {
		return nil, errors.New("github: missing X-GitHub-Delivery")
	}
	var payload struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		PullRequest *prJSON `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("github: bad payload: %w", err)
	}
	d.Action = payload.Action
	d.Repo = payload.Repository.FullName
	if payload.PullRequest != nil {
		owner, name, _ := strings.Cut(d.Repo, "/")
		pj := payload.PullRequest
		d.PR = &PullRequest{Owner: owner, Repo: name, Number: pj.Number, Title: pj.Title, Body: pj.Body, URL: pj.HTMLURL,
			BaseSHA: pj.Base.SHA, BaseRef: pj.Base.Ref, HeadSHA: pj.Head.SHA, HeadRef: pj.Head.Ref, State: pj.State}
	}
	return d, nil
}

// Relevant reports whether a delivery should (re)verify a PR head.
func (d *Delivery) Relevant() bool {
	if d.Event != "pull_request" || d.PR == nil {
		return false
	}
	switch d.Action {
	case "opened", "synchronize", "reopened", "ready_for_review":
		return true
	}
	return false
}
