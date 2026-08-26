// Package auth is the minimal private-beta authorization model: workspaces,
// users, memberships with a role, and bearer tokens. Authorization is
// decided in exactly one place — Principal.Can(action, workspace) against
// the permission matrix — and the HTTP layer resolves every resource to its
// workspace before calling it. No handler checks roles ad hoc.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Role string

const (
	RoleOwner    Role = "owner"
	RoleAdmin    Role = "admin"
	RoleMember   Role = "member"
	RoleReviewer Role = "reviewer"
	RoleViewer   Role = "viewer"
)

type Action string

const (
	ActView          Action = "view"           // cases, packets, artifacts, events, runs, decisions, effects
	ActCreate        Action = "create"         // create / import / rerun / resume / cancel a case
	ActResolve       Action = "resolve"        // answer an engine decision
	ActVerdict       Action = "verdict"        // record the human merge decision
	ActPostExternal  Action = "post_external"  // post statuses/comments to GitHub
	ActManageRepos   Action = "manage_repos"   // connect repositories, link GitHub
	ActManageMembers Action = "manage_members" // invite/remove members, tokens
)

// Matrix is the single source of truth. Build the table first, then code.
//
//	action          owner admin member reviewer viewer
//	view              ✓     ✓     ✓       ✓       ✓
//	create            ✓     ✓     ✓       ✗       ✗
//	resolve           ✓     ✓     ✓       ✗       ✗
//	verdict           ✓     ✓     ✗       ✓       ✗
//	post_external     ✓     ✓     ✗       ✓       ✗
//	manage_repos      ✓     ✓     ✗       ✗       ✗
//	manage_members    ✓     ✗     ✗       ✗       ✗
var Matrix = map[Action]map[Role]bool{
	ActView:          {RoleOwner: true, RoleAdmin: true, RoleMember: true, RoleReviewer: true, RoleViewer: true},
	ActCreate:        {RoleOwner: true, RoleAdmin: true, RoleMember: true},
	ActResolve:       {RoleOwner: true, RoleAdmin: true, RoleMember: true},
	ActVerdict:       {RoleOwner: true, RoleAdmin: true, RoleReviewer: true},
	ActPostExternal:  {RoleOwner: true, RoleAdmin: true, RoleReviewer: true},
	ActManageRepos:   {RoleOwner: true, RoleAdmin: true},
	ActManageMembers: {RoleOwner: true},
}

type Workspace struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Membership struct {
	UserID      string     `json:"user_id"`
	WorkspaceID string     `json:"workspace_id"`
	Role        Role       `json:"role"`
	CreatedAt   time.Time  `json:"created_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

type Token struct {
	ID        string     `json:"id"`
	Hash      string     `json:"hash,omitempty"` // sha256 of the secret; the secret is shown once
	UserID    string     `json:"user_id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type state struct {
	Workspaces  []Workspace  `json:"workspaces"`
	Users       []User       `json:"users"`
	Memberships []Membership `json:"memberships"`
	Tokens      []Token      `json:"tokens"`
}

// Principal is an authenticated user with their live roles.
type Principal struct {
	UserID string
	Name   string
	Roles  map[string]Role // workspace id -> role
}

// Can is the only authorization decision point.
func (p *Principal) Can(a Action, workspaceID string) bool {
	if p == nil || workspaceID == "" {
		return false
	}
	r, ok := p.Roles[workspaceID]
	if !ok {
		return false
	}
	return Matrix[a][r]
}

// Workspaces the principal belongs to.
func (p *Principal) Workspaces() []string {
	var out []string
	for w := range p.Roles {
		out = append(out, w)
	}
	return out
}

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrNotFound        = errors.New("not found")
)

// Store persists the auth state in <data>/auth.json.
type Store struct {
	path string
	mu   sync.Mutex
	st   state
}

func Open(dataDir string) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, "auth.json")}
	b, err := os.ReadFile(s.path)
	if err == nil {
		if err := json.Unmarshal(b, &s.st); err != nil {
			return nil, fmt.Errorf("auth.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// Configured reports whether any workspace exists. An unconfigured store
// means the instance runs in local single-user mode (CLI only).
func (s *Store) Configured() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.st.Workspaces) > 0
}

func (s *Store) save() error {
	b, _ := json.MarshalIndent(s.st, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func (s *Store) CreateWorkspace(name string) (Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := Workspace{ID: newID("ws"), Name: name, CreatedAt: time.Now().UTC()}
	s.st.Workspaces = append(s.st.Workspaces, w)
	return w, s.save()
}

func (s *Store) CreateUser(name string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := User{ID: newID("usr"), Name: name, CreatedAt: time.Now().UTC()}
	s.st.Users = append(s.st.Users, u)
	return u, s.save()
}

func (s *Store) SetMembership(userID, workspaceID string, role Role) error {
	if !Matrix[ActView][role] {
		return fmt.Errorf("unknown role %q", role)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Memberships {
		m := &s.st.Memberships[i]
		if m.UserID == userID && m.WorkspaceID == workspaceID {
			m.Role, m.RevokedAt = role, nil
			return s.save()
		}
	}
	s.st.Memberships = append(s.st.Memberships, Membership{UserID: userID, WorkspaceID: workspaceID, Role: role, CreatedAt: time.Now().UTC()})
	return s.save()
}

func (s *Store) RevokeMembership(userID, workspaceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for i := range s.st.Memberships {
		m := &s.st.Memberships[i]
		if m.UserID == userID && m.WorkspaceID == workspaceID && m.RevokedAt == nil {
			m.RevokedAt = &now
		}
	}
	return s.save()
}

// IssueToken creates a bearer token for a user and returns the secret once.
func (s *Store) IssueToken(userID, name string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	secret := "plt_" + hex.EncodeToString(raw)
	h := sha256.Sum256([]byte(secret))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Tokens = append(s.st.Tokens, Token{ID: newID("tok"), Hash: hex.EncodeToString(h[:]), UserID: userID, Name: name, CreatedAt: time.Now().UTC()})
	return secret, s.save()
}

// TokensOf lists a user's tokens (hashes are never returned).
func (s *Store) TokensOf(userID string) []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Token
	for _, t := range s.st.Tokens {
		if t.UserID == userID {
			t.Hash = ""
			out = append(out, t)
		}
	}
	return out
}

// Members lists memberships of a workspace with user names.
func (s *Store) Members(workspaceID string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := map[string]string{}
	for _, u := range s.st.Users {
		names[u.ID] = u.Name
	}
	var out []map[string]any
	for _, m := range s.st.Memberships {
		if m.WorkspaceID == workspaceID {
			out = append(out, map[string]any{"user_id": m.UserID, "name": names[m.UserID], "role": m.Role, "revoked": m.RevokedAt != nil})
		}
	}
	return out
}

func (s *Store) RevokeToken(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for i := range s.st.Tokens {
		if s.st.Tokens[i].ID == id {
			s.st.Tokens[i].RevokedAt = &now
		}
	}
	return s.save()
}

// Authenticate resolves a bearer secret to a principal with live roles.
// Revoked memberships are simply absent from Roles.
func (s *Store) Authenticate(bearer string) (*Principal, error) {
	bearer = strings.TrimSpace(bearer)
	if bearer == "" {
		return nil, ErrUnauthenticated
	}
	h := sha256.Sum256([]byte(bearer))
	want := hex.EncodeToString(h[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	var userID string
	for _, t := range s.st.Tokens {
		if t.RevokedAt == nil && subtle.ConstantTimeCompare([]byte(t.Hash), []byte(want)) == 1 {
			userID = t.UserID
		}
	}
	if userID == "" {
		return nil, ErrUnauthenticated
	}
	p := &Principal{UserID: userID, Roles: map[string]Role{}}
	for _, u := range s.st.Users {
		if u.ID == userID {
			p.Name = u.Name
		}
	}
	for _, m := range s.st.Memberships {
		if m.UserID == userID && m.RevokedAt == nil {
			p.Roles[m.WorkspaceID] = m.Role
		}
	}
	return p, nil
}

func (s *Store) Workspaces() []Workspace {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Workspace(nil), s.st.Workspaces...)
}
