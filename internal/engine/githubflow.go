package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"orchestrator/internal/domain"
	"orchestrator/internal/github"
	"orchestrator/internal/store"
)

// GitHub semantics live here so they can be tested against a fake server
// without any HTTP handler. Identities: repository (owner/name ↔ registered
// local mirror), PR number, base SHA, head SHA, delivery ID (idempotency),
// ChangeCase (task), packet version.

// ImportPR creates a verify-only ChangeCase for the PR's exact head SHA.
// idempotencyKey (a webhook delivery ID, or "" for manual import) suppresses
// duplicates; independent of it, the same (PR, head SHA) never yields two
// cases: the existing one is returned.
func (e *Engine) ImportPR(ctx context.Context, pr *github.PullRequest, idempotencyKey string) (*domain.Task, bool, error) {
	if e.Repos == nil {
		return nil, false, errors.New("no repository registry configured")
	}
	if pr.HeadSHA == "" || pr.BaseSHA == "" {
		return nil, false, errors.New("pull request without base/head SHA")
	}
	local, err := e.Repos.ByGitHub(pr.Owner + "/" + pr.Repo)
	if err != nil {
		return nil, false, err
	}
	// One case per exact head SHA: the semantic idempotency key.
	if existing := e.findCaseForHead(pr, pr.HeadSHA); existing != nil {
		return existing, true, nil
	}
	if !e.WS.HasCommit(ctx, local.Path, pr.HeadSHA) {
		if err := e.WS.Fetch(ctx, local.Path, pr.HeadSHA); err != nil {
			return nil, false, err
		}
	}
	if !e.WS.HasCommit(ctx, local.Path, pr.BaseSHA) {
		_ = e.WS.Fetch(ctx, local.Path, pr.BaseSHA)
	}
	goal := strings.TrimSpace(pr.Title)
	if goal == "" {
		goal = fmt.Sprintf("Verify %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
	}
	// The semantic key (PR + exact head) is what makes two deliveries the
	// same case; the delivery ID is only metadata.
	key := fmt.Sprintf("pr|%s/%s#%d|%s", pr.Owner, pr.Repo, pr.Number, pr.HeadSHA)
	ctxSrcs := nonEmpty(pr.Body)
	if idempotencyKey != "" {
		ctxSrcs = append(ctxSrcs, "github delivery "+idempotencyKey)
	}
	spec := TaskSpec{
		Goal: goal, Context: ctxSrcs, Repos: []string{local.ID},
		HeadRef: pr.HeadSHA, IdempotencyKey: key, WorkspaceID: local.Workspace, PinnedBase: pr.BaseSHA,
		PR: &domain.PullRequestRef{Owner: pr.Owner, Repo: pr.Repo, Number: pr.Number, URL: pr.URL, BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA},
	}
	return e.CreateTaskIdempotent(spec)
}

func nonEmpty(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return []string{s}
}

// findCaseForHead returns the newest task linked to the same PR head SHA.
func (e *Engine) findCaseForHead(pr *github.PullRequest, head string) *domain.Task {
	tasks, err := e.Store.ListTasks()
	if err != nil {
		return nil
	}
	var found *domain.Task
	for _, t := range tasks {
		if t.PR != nil && t.PR.Owner == pr.Owner && t.PR.Repo == pr.Repo && t.PR.Number == pr.Number && t.PR.HeadSHA == head {
			found = t
		}
	}
	return found
}

// CasesForPR lists every case of a PR, newest last, so a caller can tell
// which head SHAs were verified and which is current.
func (e *Engine) CasesForPR(owner, repo string, number int) ([]*domain.Task, error) {
	tasks, err := e.Store.ListTasks()
	if err != nil {
		return nil, err
	}
	var out []*domain.Task
	for _, t := range tasks {
		if t.PR != nil && t.PR.Owner == owner && t.PR.Repo == repo && t.PR.Number == number {
			out = append(out, t)
		}
	}
	return out, nil
}

// HandleDelivery applies one verified webhook delivery exactly once. The
// delivery ID is the idempotency key: replays return the same case and do
// not start a second run.
func (e *Engine) HandleDelivery(ctx context.Context, d *github.Delivery) (*domain.Task, bool, error) {
	if !d.Relevant() {
		return nil, false, nil
	}
	return e.ImportPR(ctx, d.PR, "delivery|"+d.ID)
}

// RefreshPR re-reads the PR. Revoked access blocks every case of the PR
// visibly; a new head SHA imports a new case and leaves old ones untouched
// (their packets stay bound to their own SHA).
func (e *Engine) RefreshPR(ctx context.Context, c *github.Client, owner, repo string, number int) (*domain.Task, error) {
	pr, err := c.FetchPR(owner, repo, number)
	if errors.Is(err, github.ErrRevoked) {
		cases, _ := e.CasesForPR(owner, repo, number)
		for _, t := range cases {
			if t.Status.Terminal() {
				continue
			}
			reason := "BLOCKED: GitHub access to " + owner + "/" + repo + " revoked or repository not visible (" + err.Error() + ")"
			if ferr := e.failTask(t, reason); errors.Is(ferr, store.ErrConflict) {
				if fresh, gerr := e.Store.GetTask(t.ID); gerr == nil && !fresh.Status.Terminal() {
					_ = e.failTask(fresh, reason)
				}
			}
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	t, _, err := e.ImportPR(ctx, pr, "")
	return t, err
}

// ---------- external effects with an at-most-once ledger ----------

var ErrEffectUnknown = errors.New("a previous attempt of this external effect may have succeeded; refusing to repeat it")

// PostGitHubStatus posts the commit status (and PR comment) derived from
// the current packet. Before the network call an intent is persisted; a
// retry that finds a pending intent refuses (UNKNOWN) instead of posting
// twice. Returns the effects recorded.
func (e *Engine) PostGitHubStatus(ctx context.Context, c *github.Client, taskID, packetURL string) ([]domain.ExternalEffect, error) {
	v, err := e.PacketState(taskID)
	if err != nil {
		return nil, err
	}
	if v.Task.PR == nil {
		return nil, errors.New("task is not linked to a pull request")
	}
	repoName := v.Task.Repos[0].Name
	sha := v.Packet.Source.HeadSHAs[repoName]
	if sha == "" {
		sha = v.Task.PR.HeadSHA
	}
	pr := v.Task.PR
	st := github.BuildStatus(v.Packet, repoName, sha, packetURL)
	comment := github.BuildComment(v.Packet, repoName, sha, packetURL)
	var out []domain.ExternalEffect
	post := func(kind, target string, do func() error) error {
		key := fmt.Sprintf("%s|%s|v%d", kind, target, v.Packet.Version)
		prev := e.lastEffect(taskID, key)
		switch {
		case prev != nil && prev.Status == "done":
			out = append(out, *prev)
			return nil // already delivered for this packet version
		case prev != nil && (prev.Status == "pending" || prev.Status == "unknown"):
			eff := *prev
			eff.Status, eff.At, eff.Detail = "unknown", time.Now().UTC(), "retry refused: previous attempt did not record an outcome"
			_ = e.Store.AddEffect(eff)
			out = append(out, eff)
			return fmt.Errorf("%w: %s", ErrEffectUnknown, key)
		}
		attempt := 1
		if prev != nil {
			attempt = prev.Attempt + 1
		}
		eff := domain.ExternalEffect{Key: key, TaskID: taskID, Kind: kind, Target: target, Status: "pending", At: time.Now().UTC(), Attempt: attempt}
		if err := e.Store.AddEffect(eff); err != nil {
			return err
		}
		if err := do(); err != nil {
			eff.Status, eff.Detail, eff.At = "failed", err.Error(), time.Now().UTC()
			_ = e.Store.AddEffect(eff)
			out = append(out, eff)
			return err
		}
		eff.Status, eff.At = "done", time.Now().UTC()
		if err := e.Store.AddEffect(eff); err != nil {
			return err
		}
		out = append(out, eff)
		return nil
	}
	if err := post("github_status", fmt.Sprintf("%s/%s@%s", pr.Owner, pr.Repo, sha), func() error {
		return c.PostStatus(ctx, pr.Owner, pr.Repo, sha, st)
	}); err != nil {
		return out, err
	}
	if err := post("github_comment", fmt.Sprintf("%s/%s#%d", pr.Owner, pr.Repo, pr.Number), func() error {
		return c.PostComment(ctx, pr.Owner, pr.Repo, pr.Number, comment)
	}); err != nil {
		return out, err
	}
	return out, nil
}

func (e *Engine) lastEffect(taskID, key string) *domain.ExternalEffect {
	effs, _ := e.Store.Effects(taskID)
	var last *domain.ExternalEffect
	for i := range effs {
		if effs[i].Key == key {
			last = &effs[i]
		}
	}
	return last
}
