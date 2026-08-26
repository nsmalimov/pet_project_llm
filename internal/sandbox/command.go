package sandbox

import (
	"errors"
	"fmt"
	"strings"
)

var ErrCommandPolicy = errors.New("command rejected by policy")

// SplitWords splits a command line into argv with POSIX-ish quoting
// (single quotes literal, double quotes allow \" escapes). It rejects
// everything that would need a shell: pipes, redirections, substitution,
// separators, globs, newlines.
func SplitWords(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	in := false
	quote := rune(0)
	esc := false
	flush := func() {
		if in {
			argv = append(argv, cur.String())
			cur.Reset()
			in = false
		}
	}
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				esc = true
			case '$', '`':
				return nil, fmt.Errorf("%w: shell expansion %q not allowed", ErrCommandPolicy, string(r))
			default:
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			in = true
		case r == '\\':
			esc = true
			in = true
		case r == ' ' || r == '\t':
			flush()
		case strings.ContainsRune("|&;<>()`$*?[]{}~\n\r#", r):
			return nil, fmt.Errorf("%w: shell metacharacter %q not allowed", ErrCommandPolicy, string(r))
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 || esc {
		return nil, fmt.Errorf("%w: unterminated quote", ErrCommandPolicy)
	}
	flush()
	if len(argv) == 0 {
		return nil, fmt.Errorf("%w: empty command", ErrCommandPolicy)
	}
	return argv, nil
}

// ValidateCommand turns an untrusted command string into argv under the
// runner allowlist. argv[0] must be a bare runner name (no path components),
// so PATH lookup inside the constructed environment decides what runs.
func (p Policy) ValidateCommand(cmd string) ([]string, error) {
	argv, err := SplitWords(strings.TrimSpace(cmd))
	if err != nil {
		return nil, err
	}
	runner := argv[0]
	allowed := false
	for _, r := range p.Runners {
		if runner == r {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("%w: runner %q not in allowlist (%s)", ErrCommandPolicy, runner, strings.Join(p.Runners, ","))
	}
	if runner != "./gradlew" && strings.ContainsAny(runner, "/\\") {
		return nil, fmt.Errorf("%w: runner must be a bare name, got %q", ErrCommandPolicy, runner)
	}
	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "--") {
			a = a[1:] // "--exec=" and "-exec=" are the same flag to go
		}
		if strings.HasPrefix(a, "-exec") || a == "-toolexec" || strings.HasPrefix(a, "-toolexec=") || strings.HasPrefix(a, "-exec=") {
			return nil, fmt.Errorf("%w: %q would execute an arbitrary program", ErrCommandPolicy, a)
		}
		if strings.HasPrefix(a, "-c") && (runner == "python3" || runner == "python") {
			return nil, fmt.Errorf("%w: python -c is arbitrary code", ErrCommandPolicy)
		}
		if a == "-ldflags" || strings.HasPrefix(a, "-ldflags=") || a == "-gcflags" || strings.HasPrefix(a, "-gcflags=") {
			return nil, fmt.Errorf("%w: %q not allowed", ErrCommandPolicy, a)
		}
	}
	return argv, nil
}
