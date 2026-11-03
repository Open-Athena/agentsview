// Package publish prepares a redacted snapshot of selected sessions for
// sharing: it exports them, finds secrets with several detectors, reports the
// findings, and (when writing) replaces each one with a [REDACTED:<kind>]
// token. Secret values are held in memory only and never logged or printed.
package publish

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// secretName matches environment variable names that hold credentials.
var secretName = regexp.MustCompile(
	`(?i)(KEY|SECRET|TOKEN|_PAT$|^PAT$|PASS|PASSWORD|CREDENTIAL|AUTH|PRIVATE|SESSION|COOKIE|SIGNING|WEBHOOK|DSN)`)

// identifierName matches names of identifiers and locations (account IDs,
// URLs, bucket names), which are not credentials on their own.
var identifierName = regexp.MustCompile(
	`(?i)(_ID$|_URL$|_HOST$|_PORT$|_PATH$|_DIR$|_FILE$|_NAME$|_REGION$|_ENDPOINT$|_BUCKET$|PUBLIC|^BASH_FUNC_|^DIRENV_)`)

var plainWord = regexp.MustCompile(`^[a-z_.-]+$`)

// isIdentifierName reports whether a variable names an identifier rather than
// a credential. An access key's ID still counts as a credential: its name
// contains KEY.
func isIdentifierName(name string) bool {
	return identifierName.MatchString(name) &&
		!strings.Contains(strings.ToUpper(name), "KEY")
}

// isSecretName reports whether a variable name looks like it holds a secret.
func isSecretName(name string) bool {
	return secretName.MatchString(name) && !isIdentifierName(name)
}

// credentialShaped reports whether a value looks like a generated credential:
// long, mixing letters and digits, with no path separators or spaces.
func credentialShaped(v string) bool {
	return len(v) >= 20 && !strings.ContainsAny(v, "/ ") &&
		strings.ContainsAny(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") &&
		strings.ContainsAny(v, "0123456789")
}

// keepEnvValue decides whether an environment variable's value is a secret
// worth redacting everywhere it appears. Short or plain-word values ("true",
// "us-east-1") would redact ordinary prose, so they never count.
func keepEnvValue(name, value string) bool {
	if len(value) < 12 || strings.HasPrefix(value, "$") || plainWord.MatchString(value) {
		return false
	}
	if isIdentifierName(name) {
		return false
	}
	return secretName.MatchString(name) || credentialShaped(value)
}

// EnvReport counts how the session directories' environments were resolved.
type EnvReport struct {
	Dirs     int `json:"dirs"`     // distinct session directories considered
	Direnv   int `json:"direnv"`   // resolved through `direnv exec`
	FileOnly int `json:"fileOnly"` // read from .envrc / .env* / .dev.vars files only
	Missing  int `json:"missing"`  // not present on this machine
	Values   int `json:"values"`   // distinct secret values collected
}

// EnvOptions configures environment collection.
type EnvOptions struct {
	// Direnv resolves each directory through `direnv exec <dir> env`, which
	// follows source_up and secret-manager reads. When false, or when direnv
	// is not installed or refuses the directory, the .envrc file is parsed
	// instead.
	Direnv bool
}

// CollectEnv gathers secret values from the environment of each directory:
// what direnv adds there (minus the baseline environment outside any
// .envrc), plus .dev.vars and .env* files. It returns value -> kind, where
// kind is "env:<NAME>".
func CollectEnv(ctx context.Context, dirs []string, opts EnvOptions) (map[string]string, EnvReport) {
	out := map[string]string{}
	rep := EnvReport{}
	useDirenv := opts.Direnv
	if useDirenv {
		if _, err := exec.LookPath("direnv"); err != nil {
			useDirenv = false
		}
	}
	var baseline map[[2]string]struct{}
	if useDirenv {
		pairs, err := direnvEnv(ctx, string(filepath.Separator))
		if err != nil {
			useDirenv = false
		} else {
			baseline = make(map[[2]string]struct{}, len(pairs))
			for _, kv := range pairs {
				baseline[kv] = struct{}{}
			}
		}
	}

	seen := map[string]struct{}{}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		rep.Dirs++
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			rep.Missing++
			continue
		}

		var pairs [][2]string
		resolved := false
		if useDirenv {
			if env, err := direnvEnv(ctx, dir); err == nil {
				for _, kv := range env {
					if _, ok := baseline[kv]; !ok {
						pairs = append(pairs, kv)
					}
				}
				resolved = true
			}
		}
		files := []string{filepath.Join(dir, ".dev.vars"), filepath.Join(dir, ".env")}
		if extra, err := filepath.Glob(filepath.Join(dir, ".env.*")); err == nil {
			files = append(files, extra...)
		}
		if resolved {
			rep.Direnv++
		} else {
			rep.FileOnly++
			files = append(files, filepath.Join(dir, ".envrc"))
		}
		for _, f := range files {
			pairs = append(pairs, parseEnvFile(f)...)
		}
		for _, kv := range pairs {
			if keepEnvValue(kv[0], kv[1]) {
				if _, ok := out[kv[1]]; !ok {
					out[kv[1]] = "env:" + kv[0]
				}
			}
		}
	}
	rep.Values = len(out)
	return out, rep
}

// direnvEnv returns the environment direnv resolves for dir. Its stderr
// (load messages, warnings) is discarded; nothing is echoed.
func direnvEnv(ctx context.Context, dir string) ([][2]string, error) {
	cmd := exec.CommandContext(ctx, "direnv", "exec", dir, "env", "-0")
	cmd.Stderr = nil
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var out [][2]string
	for kv := range bytes.SplitSeq(b, []byte{0}) {
		k, v, ok := strings.Cut(string(kv), "=")
		if ok && k != "" {
			out = append(out, [2]string{k, v})
		}
	}
	return out, nil
}

// parseEnvFile reads KEY=VALUE lines (optionally prefixed with `export`),
// skipping comments. A missing file yields nothing.
func parseEnvFile(path string) [][2]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][2]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k == "" || strings.ContainsAny(k, " \t") {
			continue
		}
		out = append(out, [2]string{k, v})
	}
	return out
}

// LoadDenyList reads one term per line (blank lines and # comments skipped).
// A missing file is an empty list.
func LoadDenyList(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var terms []string
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			terms = append(terms, line)
		}
	}
	slices.Sort(terms)
	return slices.Compact(terms), nil
}
