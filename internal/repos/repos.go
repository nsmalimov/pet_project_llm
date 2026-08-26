// Package repos is the registry of source repositories a Proofline instance
// may operate on. Tasks reference repositories by ID; raw filesystem paths
// from API input are only accepted in LOCAL_UNSAFE mode and are canonicalised
// and validated even then.
package repos

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"orchestrator/internal/domain"
	"orchestrator/internal/sandbox"
)

type Repo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"` // canonical
	AddedAt   time.Time `json:"added_at"`
	Workspace string    `json:"workspace,omitempty"` // authorization scope
	GitHub    string    `json:"github,omitempty"`    // owner/name this local clone mirrors
	// Policy is the approved way to verify this repository. Public requests
	// may not override it; SAFE mode requires it.
	Policy *Policy `json:"policy,omitempty"`
}

// Policy is repository-level verification policy.
type Policy struct {
	TestCommand   string            `json:"test_command,omitempty"`
	ReproCommand  string            `json:"repro_command,omitempty"`
	Integration   *IntegrationCheck `json:"integration,omitempty"`
	MaxWorktreeMB int               `json:"max_worktree_mb,omitempty"`
}

// IntegrationCheck starts the service from the worktree and probes it.
type IntegrationCheck struct {
	Start          string      `json:"start"`           // approved argv command, e.g. "go run ./cmd/server"
	Port           int         `json:"port"`            // service listens on 127.0.0.1:port (PORT env is set)
	StartupSeconds int         `json:"startup_seconds"` // wait for the port
	Checks         []HTTPCheck `json:"checks"`
}

type HTTPCheck struct {
	Name         string `json:"name"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	Body         string `json:"body,omitempty"`
	ExpectStatus int    `json:"expect_status"`
	ExpectBody   string `json:"expect_body,omitempty"` // substring
}

type Registry struct {
	path   string
	policy sandbox.Policy
	mu     sync.Mutex
}

var ErrNotRegistered = errors.New("repository not registered")

func Open(dataDir string, p sandbox.Policy) *Registry {
	return &Registry{path: filepath.Join(dataDir, "repos.json"), policy: p}
}

func (r *Registry) load() ([]Repo, error) {
	b, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Repo
	return out, json.Unmarshal(b, &out)
}

func (r *Registry) save(list []Repo) error {
	b, _ := json.MarshalIndent(list, "", "  ")
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// Validate canonicalises a repository path and checks the policy: it must be
// a git repository (has .git), must not be inside the workspace root (a task
// worktree is not a source repo), and in SAFE mode must be under one of the
// configured ReposRoots.
func (r *Registry) Validate(path string) (string, error) {
	c, err := sandbox.Canonical(path)
	if err != nil {
		return "", fmt.Errorf("repository path: %w", err)
	}
	if sandbox.Under(r.policy.WorkspaceRoot, c) {
		return "", fmt.Errorf("repository %s is inside the Proofline workspace; refusing", c)
	}
	if _, err := os.Stat(filepath.Join(c, ".git")); err != nil {
		return "", fmt.Errorf("%s is not a git repository", c)
	}
	if len(r.policy.ReposRoots) > 0 {
		ok := false
		for _, root := range r.policy.ReposRoots {
			if sandbox.Under(root, c) {
				ok = true
			}
		}
		if !ok {
			return "", fmt.Errorf("repository %s is outside the allowed roots %v", c, r.policy.ReposRoots)
		}
	} else if r.policy.Mode == sandbox.ModeSafe {
		return "", errors.New("SAFE_SANDBOX requires --repos-root; refusing an unconstrained repository path")
	}
	return c, nil
}

// SetGitHub links a registered repo to its GitHub full name (owner/name).
func (r *Registry) SetGitHub(id, fullName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.load()
	if err != nil {
		return err
	}
	for i := range list {
		if list[i].ID == id {
			list[i].GitHub = strings.ToLower(fullName)
			return r.save(list)
		}
	}
	return fmt.Errorf("%w: %s", ErrNotRegistered, id)
}

// SetPolicy replaces the repository policy after validating its commands.
func (r *Registry) SetPolicy(id string, pol *Policy) error {
	if pol != nil {
		for _, c := range []string{pol.TestCommand, pol.ReproCommand} {
			if c != "" {
				if _, err := r.policy.ValidateCommand(c); err != nil {
					return err
				}
			}
		}
		if pol.Integration != nil {
			if _, err := r.policy.ValidateCommand(pol.Integration.Start); err != nil {
				return fmt.Errorf("integration start: %w", err)
			}
			if pol.Integration.Port <= 0 || pol.Integration.Port > 65535 || len(pol.Integration.Checks) == 0 {
				return errors.New("integration check needs a port and at least one check")
			}
			for _, c := range pol.Integration.Checks {
				if c.Name == "" || !strings.HasPrefix(c.Path, "/") || c.ExpectStatus <= 0 {
					return fmt.Errorf("integration check %q: name, path starting with / and expect_status are required", c.Name)
				}
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.load()
	if err != nil {
		return err
	}
	for i := range list {
		if list[i].ID == id {
			list[i].Policy = pol
			return r.save(list)
		}
	}
	return fmt.Errorf("%w: %s", ErrNotRegistered, id)
}

// ByGitHub finds the registered repo mirroring owner/name.
func (r *Registry) ByGitHub(fullName string) (Repo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.load()
	if err != nil {
		return Repo{}, err
	}
	for _, x := range list {
		if x.GitHub != "" && x.GitHub == strings.ToLower(fullName) {
			return x, nil
		}
	}
	return Repo{}, fmt.Errorf("%w: no local repository is linked to github %s", ErrNotRegistered, fullName)
}

func (r *Registry) Add(path, workspace string) (Repo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.Validate(path)
	if err != nil {
		return Repo{}, err
	}
	list, err := r.load()
	if err != nil {
		return Repo{}, err
	}
	for _, x := range list {
		if x.Path == c && x.Workspace == workspace {
			return x, nil
		}
	}
	rp := Repo{ID: domain.NewID("repo"), Name: filepath.Base(c), Path: c, AddedAt: time.Now().UTC(), Workspace: workspace}
	list = append(list, rp)
	return rp, r.save(list)
}

func (r *Registry) List() ([]Repo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.load()
	sort.Slice(list, func(i, j int) bool { return list[i].AddedAt.Before(list[j].AddedAt) })
	return list, err
}

func (r *Registry) Get(id string) (Repo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.load()
	if err != nil {
		return Repo{}, err
	}
	for _, x := range list {
		if x.ID == id {
			return x, nil
		}
	}
	return Repo{}, fmt.Errorf("%w: %s", ErrNotRegistered, id)
}

// Resolve turns a task input (repo ID or, in LOCAL_UNSAFE, a raw path) into
// a canonical, validated path.
func (r *Registry) Resolve(ref string) (string, error) {
	if strings.HasPrefix(ref, "repo_") {
		rp, err := r.Get(ref)
		if err != nil {
			return "", err
		}
		// Re-validate: the path may have been replaced by a symlink since.
		return r.Validate(rp.Path)
	}
	if r.policy.Mode == sandbox.ModeSafe {
		return "", fmt.Errorf("SAFE_SANDBOX accepts registered repository IDs only, got path %q (use `orc repo add`)", ref)
	}
	return r.Validate(ref)
}
