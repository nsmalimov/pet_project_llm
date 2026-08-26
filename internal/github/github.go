// Package github turns a proof packet into a GitHub commit status / PR
// comment. It never reports a fake approval: only a SUPPORTED packet bound
// to exactly the SHA being reported yields "success"; anything else is a
// failure state with an explanation. Posting requires GITHUB_TOKEN; without
// it the payloads are still built (and printed by the CLI) so the mapping is
// testable and auditable offline.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"orchestrator/internal/domain"
)

const Context = "proofline/change-assurance"

// CommitStatus is the payload of POST /repos/{owner}/{repo}/statuses/{sha}.
type CommitStatus struct {
	State       string `json:"state"` // success | failure | error | pending
	Description string `json:"description"`
	Context     string `json:"context"`
	TargetURL   string `json:"target_url,omitempty"`
}

// BuildStatus maps a packet to the status for one specific commit. If the
// packet was not built on that exact SHA the status says NOT VERIFIED —
// a newer push invalidates every earlier verification.
func BuildStatus(p *domain.Packet, repoName, sha, packetURL string) CommitStatus {
	st := CommitStatus{Context: Context, TargetURL: packetURL}
	if p == nil {
		st.State, st.Description = "failure", "PROOFLINE: NOT VERIFIED — no proof packet for "+short(sha)
		return st
	}
	head := p.Source.HeadSHAs[repoName]
	if head == "" || head != sha || p.Source.Dirty {
		st.State = "failure"
		st.Description = fmt.Sprintf("PROOFLINE: NOT VERIFIED for %s (last packet v%d is for %s)", short(sha), p.Version, short(head))
		return st
	}
	sup, core := 0, 0
	for _, c := range p.Claims {
		if c.Core {
			core++
			if c.Status == domain.ClaimSupported {
				sup++
			}
		}
	}
	summary := fmt.Sprintf("%d/%d claims supported, %d not verified, %d risk(s)", sup, core, len(p.Gaps), len(p.Risks))
	switch p.Verdict {
	case domain.ClaimSupported:
		st.State = "success"
	default:
		st.State = "failure"
	}
	st.Description = truncate("PROOFLINE: "+strings.ToUpper(string(p.Verdict))+" — "+summary, 140)
	return st
}

// BuildComment renders the PR comment. It links to the packet and never
// claims approval; the human decision is recorded in Proofline, not here.
func BuildComment(p *domain.Packet, repoName, sha, packetURL string) string {
	st := BuildStatus(p, repoName, sha, packetURL)
	var sb strings.Builder
	fmt.Fprintf(&sb, "**%s**\n\n", st.Description)
	if p != nil && p.Source.HeadSHAs[repoName] == sha {
		for _, c := range p.Claims {
			fmt.Fprintf(&sb, "- `%s` %s — %s\n", strings.ToUpper(string(c.Status)), c.Title, c.Statement)
		}
		if len(p.Contradictions) > 0 {
			sb.WriteString("\n**Contradictions**\n")
			for _, x := range p.Contradictions {
				sb.WriteString("- " + x + "\n")
			}
		}
		if len(p.Gaps) > 0 {
			sb.WriteString("\n**Not verified**\n")
			for _, g := range p.Gaps {
				sb.WriteString("- " + g + "\n")
			}
		}
	}
	fmt.Fprintf(&sb, "\n[View Proof Packet →](%s)\n\n_This is a change-assurance record, not an approval. The merge decision is a human decision recorded in Proofline._\n", packetURL)
	return sb.String()
}

// Client posts to the GitHub REST API. Token is a fine-grained PAT or App
// installation token with `statuses:write` (+ `pull_requests:write` for
// comments).
type Client struct {
	Token   string
	BaseURL string // default https://api.github.com
	HTTP    *http.Client
}

func (c *Client) do(ctx context.Context, method, path string, body any) error {
	if c.Token == "" {
		return fmt.Errorf("github: no token (set GITHUB_TOKEN)")
	}
	base := c.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("github: %s %s → %d: %s", method, path, resp.StatusCode, truncate(buf.String(), 300))
	}
	return nil
}

func (c *Client) PostStatus(ctx context.Context, owner, repo, sha string, st CommitStatus) error {
	return c.do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/statuses/%s", owner, repo, sha), st)
}

func (c *Client) PostComment(ctx context.Context, owner, repo string, number int, body string) error {
	return c.do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number), map[string]string{"body": body})
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	if s == "" {
		return "?"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
