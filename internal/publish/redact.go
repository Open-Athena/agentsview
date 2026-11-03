package publish

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/secrets"
)

// Action is what a publish does with a finding.
type Action string

const (
	// ActionRedact replaces the finding with a [REDACTED:<kind>] token.
	ActionRedact Action = "redact"
	// ActionFlag reports the finding for review and leaves the text alone.
	ActionFlag Action = "flag"
)

// KindDeny is the finding kind for deny-list terms.
const KindDeny = "deny"

// highEntropyRule is the scanner's catch-all assignment rule. Nearly all of
// its candidates are hashes, paths and IDs, so only those whose variable
// name looks secret are redacted; the rest are counted as ignored.
const highEntropyRule = "high-entropy-assignment"

// redactCandidates are candidate rules precise enough to redact by default.
var redactCandidates = map[string]bool{
	"jwt":               true,
	"basic-auth-url":    true,
	"private-key-block": true,
}

// assignmentName captures the variable name an assignment's value follows.
var assignmentName = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*[=:]\s*['"]?$`)

// assignmentLookback bounds how far before a value assignmentName looks for
// its variable name.
const assignmentLookback = 160

// Token is the replacement text for a redacted value of the given kind.
func Token(kind string) string { return "[REDACTED:" + kind + "]" }

// location identifies where in the export a string came from.
type location struct {
	file      string
	sessionID string
	ordinal   int // -1 when the string is not part of a message
	route     string
	field     string
}

// hit is one recorded finding occurrence.
type hit struct {
	loc    location
	kind   string
	action Action
}

// Redactor applies the detectors to strings and records what it finds.
type Redactor struct {
	needles map[string]string // literal (raw and JSON-escaped) -> kind
	ordered []string          // needles, longest first
	deny    []string          // lower-cased deny-list terms
	hits    []hit
	ignored map[string]int
}

// NewRedactor builds a redactor from collected secret values (value -> kind)
// and deny-list terms.
func NewRedactor(values map[string]string, deny []string) *Redactor {
	r := &Redactor{needles: map[string]string{}, ignored: map[string]int{}}
	for v, k := range values {
		r.needles[v] = k
		if esc := jsonEscaped(v); esc != v {
			r.needles[esc] = k
		}
	}
	if len(r.needles) > 0 {
		keys := make([]string, 0, len(r.needles))
		for k := range r.needles {
			keys = append(keys, k)
		}
		// Longest first, so a value containing another is replaced whole.
		slices.SortFunc(keys, func(a, b string) int {
			if len(a) != len(b) {
				return len(b) - len(a)
			}
			return strings.Compare(a, b)
		})
		r.ordered = keys
	}
	for _, t := range deny {
		r.deny = append(r.deny, strings.ToLower(t))
	}
	return r
}

// jsonEscaped returns v as it appears inside a JSON string literal.
func jsonEscaped(v string) string {
	b, err := jsontext.AppendQuote(nil, v)
	if err != nil {
		return v
	}
	return string(b[1 : len(b)-1])
}

// apply runs every detector over s, recording findings at loc, and returns
// the text with redact-action findings replaced.
func (r *Redactor) apply(s string, loc location, record bool) string {
	note := func(kind string, action Action) {
		if record {
			r.hits = append(r.hits, hit{loc: loc, kind: kind, action: action})
		}
	}
	for _, n := range r.ordered {
		if c := strings.Count(s, n); c > 0 {
			kind := r.needles[n]
			for range c {
				note(kind, ActionRedact)
			}
			s = strings.ReplaceAll(s, n, Token(kind))
		}
	}

	matches := secrets.Scan(s)
	type span struct {
		start, end int
		kind       string
	}
	var spans []span
	for _, m := range matches {
		switch {
		case m.Rule == highEntropyRule:
			name := assignmentName.FindStringSubmatch(s[max(0, m.Start-assignmentLookback):m.Start])
			if name == nil || !isSecretName(name[1]) {
				if record {
					r.ignored[m.Rule]++
				}
				continue
			}
			note(m.Rule, ActionRedact)
			spans = append(spans, span{m.Start, m.End, m.Rule})
		case m.Confidence == secrets.ConfidenceDefinite || redactCandidates[m.Rule]:
			note(m.Rule, ActionRedact)
			spans = append(spans, span{m.Start, m.End, m.Rule})
		default:
			note(m.Rule, ActionFlag)
		}
	}
	// Replace from the end so earlier offsets stay valid; skip any span that
	// overlaps one already replaced.
	slices.SortFunc(spans, func(a, b span) int { return b.start - a.start })
	limit := len(s)
	for _, sp := range spans {
		if sp.end > limit {
			continue
		}
		s = s[:sp.start] + Token(sp.kind) + s[sp.end:]
		limit = sp.start
	}

	if len(r.deny) > 0 {
		lower := strings.ToLower(s)
		for _, t := range r.deny {
			for range strings.Count(lower, t) {
				note(KindDeny, ActionFlag)
			}
		}
	}
	return s
}

// Scrub returns s with redact-action findings replaced, without recording
// anything. Used for text shown in the report (session titles).
func (r *Redactor) Scrub(s string) string { return r.apply(s, location{}, false) }

// rowLocation is the subset of an export row that locates it.
type rowLocation struct {
	ID        any    `json:"id"`
	SessionID string `json:"session_id"`
	Ordinal   *int   `json:"ordinal"`
	MessageID *int64 `json:"message_id"`
	Route     string `json:"route"`
}

// ProcessDir runs the redactor over every *.ndjson file in an export
// directory. When write is true, files with redactions are rewritten in
// place; otherwise the directory is left untouched.
func (r *Redactor) ProcessDir(dir string, write bool) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.ndjson"))
	if err != nil {
		return err
	}
	// messages before tool_calls, so tool calls can be located by ordinal.
	slices.Sort(files)
	msgOrdinal := map[int64]int{}
	for _, f := range files {
		if err := r.processFile(f, write, msgOrdinal); err != nil {
			return err
		}
	}
	return nil
}

func (r *Redactor) processFile(path string, write bool, msgOrdinal map[int64]int) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	base := filepath.Base(path)

	var out bytes.Buffer
	changed := false
	rd := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			body := bytes.TrimRight(line, "\n")
			newBody, ch, perr := r.processLine(base, body, msgOrdinal)
			if perr != nil {
				return fmt.Errorf("publish: %s: %w", base, perr)
			}
			changed = changed || ch
			out.Write(newBody)
			out.WriteByte('\n')
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if write && changed {
		return os.WriteFile(path, out.Bytes(), 0o644)
	}
	return nil
}

// processLine rewrites the string values of one NDJSON row. Object names and
// non-string values pass through verbatim; an unchanged row is returned as is.
func (r *Redactor) processLine(file string, line []byte, msgOrdinal map[int64]int) ([]byte, bool, error) {
	if len(bytes.TrimSpace(line)) == 0 {
		return line, false, nil
	}
	var rl rowLocation
	if err := json.Unmarshal(line, &rl, jsontext.AllowInvalidUTF8(true)); err != nil {
		return nil, false, fmt.Errorf("decode row: %w", err)
	}
	loc := location{file: file, ordinal: -1, route: rl.Route}
	switch file {
	case "sessions.ndjson":
		if id, ok := rl.ID.(string); ok {
			loc.sessionID = id
		}
	case "messages.ndjson":
		loc.sessionID = rl.SessionID
		if rl.Ordinal != nil {
			loc.ordinal = *rl.Ordinal
			if id, ok := rl.ID.(float64); ok {
				msgOrdinal[int64(id)] = *rl.Ordinal
			}
		}
	case "tool_calls.ndjson":
		loc.sessionID = rl.SessionID
		if rl.MessageID != nil {
			if ord, ok := msgOrdinal[*rl.MessageID]; ok {
				loc.ordinal = ord
			}
		}
	}

	dec := jsontext.NewDecoder(bytes.NewReader(line), jsontext.AllowInvalidUTF8(true))
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf, jsontext.AllowInvalidUTF8(true))
	changed := false
	for {
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, fmt.Errorf("tokenize row: %w", err)
		}
		if tok.Kind() == '"' {
			depth := dec.StackDepth()
			kind, n := dec.StackIndex(depth)
			isName := kind == '{' && n%2 == 1
			switch {
			case isName && depth == 1:
				loc.field = tok.String()
			case !isName:
				s := tok.String()
				if ns := r.apply(s, loc, true); ns != s {
					tok = jsontext.String(ns)
					changed = true
				}
			}
		}
		if err := enc.WriteToken(tok); err != nil {
			return nil, false, fmt.Errorf("encode row: %w", err)
		}
	}
	if !changed {
		return line, false, nil
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), true, nil
}

// VerifyDir fails if any collected value (raw or JSON-escaped) survives in
// any file of the directory. The error names the file, never the value.
func (r *Redactor) VerifyDir(dir string) error {
	if len(r.ordered) == 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		for _, n := range r.ordered {
			if bytes.Contains(b, []byte(n)) {
				return fmt.Errorf("publish: a collected secret value survived redaction in %s", e.Name())
			}
		}
	}
	return nil
}
