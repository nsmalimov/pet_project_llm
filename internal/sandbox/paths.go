package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrEscape = errors.New("path escapes its allowed root")

// Canonical resolves symlinks and returns an absolute clean path. The path
// must exist.
func Canonical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	c, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(c), nil
}

// Under reports whether canonical path p is root or inside root.
func Under(root, p string) bool {
	root = filepath.Clean(root)
	p = filepath.Clean(p)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// CanonicalUnder canonicalises p and requires it to be under root.
func CanonicalUnder(root, p string) (string, error) {
	c, err := Canonical(p)
	if err != nil {
		return "", err
	}
	if !Under(root, c) {
		return "", fmt.Errorf("%w: %s not under %s", ErrEscape, c, root)
	}
	return c, nil
}

// RelativeInside validates a repo-relative path from untrusted input (agent
// output, scenario files, API): no absolute paths, no "..", no NUL, and the
// resolved file (if it exists) must stay under root even through symlinks.
func RelativeInside(root, rel string) (string, error) {
	if rel == "" || strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: empty or NUL path", ErrEscape)
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "~") {
		return "", fmt.Errorf("%w: absolute path %q", ErrEscape, rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrEscape, rel)
	}
	full := filepath.Join(root, clean)
	// Walk existing ancestors through symlinks.
	probe := full
	for {
		if c, err := filepath.EvalSymlinks(probe); err == nil {
			if !Under(root, c) {
				return "", fmt.Errorf("%w: %q resolves to %s", ErrEscape, rel, c)
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	return full, nil
}

// Violation is one finding of ScanWorktree.
type Violation struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ScanWorktree inspects a checked-out tree for content a hostile repository
// can use to escape: symlinks pointing outside the tree, submodules (never
// initialised, but their presence means the tree is incomplete), and
// hook-like executables in .git. Also enforces the size cap.
func ScanWorktree(root string, maxBytes int64) ([]Violation, error) {
	root = filepath.Clean(root)
	var out []Violation
	var size int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" && p != root {
			// Nested .git (worktree marker file or a nested repo): skip the
			// contents, but a nested repository is itself suspicious.
			if d.IsDir() {
				out = append(out, Violation{p, "nested git repository"})
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			resolved := target
			if !filepath.IsAbs(target) {
				resolved = filepath.Join(filepath.Dir(p), target)
			}
			resolved = filepath.Clean(resolved)
			if !Under(root, resolved) {
				out = append(out, Violation{p, "symlink escapes the worktree → " + target})
			}
			return nil
		}
		if d.Name() == ".gitmodules" && filepath.Dir(p) == root {
			out = append(out, Violation{p, "submodules are not supported (content outside the verified tree)"})
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
				if maxBytes > 0 && size > maxBytes {
					out = append(out, Violation{root, fmt.Sprintf("worktree exceeds %d bytes", maxBytes)})
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	return out, err
}
