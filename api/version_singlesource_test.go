package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "go-xbridge/version"
)

// Single-source-of-truth enforcement for every version number.
//
// This file contains no version numbers by design: it reads each family's
// literal out of version/version.go at runtime and enforces that the literal
// appears nowhere else. Changing a default touches exactly one file (plus the
// conformance signature vectors that sign version-bearing packet bytes).
// A deliberate default change is detected by the conformance suite, whose
// signature vectors were produced under the previous default.

// homeFile is the single definition site, repo-root relative.
const homeFile = "version/version.go"

// constDecl matches a top-level single-line const definition and captures its
// name and literal (quoted string or bare number). It deliberately does not
// match var lines or functions.
var constDecl = regexp.MustCompile(`(?m)^const\s+(\w+)\s+(?:[\w.]+\s+)?=\s+("[^"]*"|\d+)\s*$`)

// family is one const name with the literal read from the single source.
type family struct {
	name string
	lit  string // raw literal text, quotes included for strings
}

// short reports whether lit is too short to scan verbatim (it would match
// line citations, command numbers, and ports). Short literals are only
// matched in version-statement position (see versionStmt).
func (f family) short() bool {
	v := strings.Trim(f.lit, `"`)
	if len(v) >= 3 {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// versionStmt matches a version assignment for a short literal: a symbol name
// containing Version assigned the extracted value, e.g. a struct field or a
// const definition. Built from the extracted value, never hardcoded.
func (f family) versionStmt() *regexp.Regexp {
	return regexp.MustCompile(`Version\w*\s*[:=]+\s*` + regexp.QuoteMeta(strings.Trim(f.lit, `"`)) + `\b`)
}

// readFamilies parses the const literals out of the single source.
func readFamilies(t *testing.T) []family {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", homeFile))
	if err != nil {
		t.Fatalf("read %s: %v", homeFile, err)
	}
	var out []family
	for _, m := range constDecl.FindAllStringSubmatch(string(raw), -1) {
		out = append(out, family{name: m[1], lit: m[2]})
	}
	if len(out) == 0 {
		t.Fatalf("no const literals parsed from %s (single source unreadable?)", homeFile)
	}
	return out
}

// docsPatterns builds the docs-side tripwires for a family from its extracted
// value: exact matches for distinctive literals, normative-statement patterns
// for short ones. C++ line citations, command numbers, and port references
// never match these.
func docsPatterns(f family) []*regexp.Regexp {
	if !f.short() {
		return []*regexp.Regexp{regexp.MustCompile(regexp.QuoteMeta(f.lit))}
	}
	v := strings.Trim(f.lit, `"`)
	var out []*regexp.Regexp
	switch {
	case strings.Contains(f.name, "XBridge"):
		out = append(out,
			regexp.MustCompile(`XBRIDGE_PROTOCOL_VERSION[^0-9]{0,16}`+v),
			regexp.MustCompile(`differs from `+v),
		)
	case strings.Contains(f.name, "XRouter"):
		out = append(out,
			regexp.MustCompile(`XROUTER_PROTOCOL_VERSION[^0-9]{0,16}`+v),
			regexp.MustCompile(`XRouter `+v),
		)
	}
	return out
}

// frozenDocsLines are the docs lines allowed to match a tripwire:
// live-capture observations, identified by number-free fragments. Matched by
// substring so benign re-wrapping does not create false failures; any NEW
// literal must either derive from a symbol or add an allowlist entry with a
// frozen-capture comment.
var frozenDocsLines = map[string][]string{
	filepath.Join("docs", "protocol.md"): {
		"the peer returned",              // live handshake observation (capture)
		"and immediately began emitting", // same captured observation, next line
	},
}

// TestVersionsSingleSourced enforces the invariant: each literal parsed from
// the single source appears exactly once there (its definition) and nowhere
// else in production code or docs (outside frozen-capture lines). A stray is
// a regression toward hardcoded versions.
func TestVersionsSingleSourced(t *testing.T) {
	families := readFamilies(t)
	seen := map[string]int{}
	for _, f := range families {
		seen[f.lit]++
	}
	for lit, n := range seen {
		if n > 1 {
			t.Errorf("literal %q defined %d times in %s (single source of truth)", lit, n, homeFile)
		}
	}

	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if samePath(path, filepath.Join("..", homeFile)) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			for _, f := range families {
				if f.short() {
					if loc := f.versionStmt().FindString(line); loc != "" {
						t.Errorf("%s:%d: stray version statement %q outside %s (single source of truth)",
							path, i+1, loc, homeFile)
					}
					continue
				}
				if strings.Contains(line, f.lit) {
					t.Errorf("%s:%d: stray version literal %q outside %s (single source of truth)",
						path, i+1, f.lit, homeFile)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	docs := []string{
		filepath.Join("..", "README.md"),
		filepath.Join("..", "docs", "protocol.md"),
		filepath.Join("..", "docs", "architecture.md"),
	}
	for _, doc := range docs {
		raw, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			for _, f := range families {
				for _, re := range docsPatterns(f) {
					loc := re.FindString(line)
					if loc == "" || allowedDocsLine(doc, line) {
						continue
					}
					t.Errorf("%s:%d: version literal %q in docs outside a frozen-capture annotation (reference the Go symbol instead)",
						doc, i+1, loc)
				}
			}
		}
	}
}

// allowedDocsLine reports whether a docs line matching a tripwire is an
// explicitly allowlisted frozen-capture line.
func allowedDocsLine(doc, line string) bool {
	rel := doc
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		rel = strings.TrimPrefix(rel, ".."+string(filepath.Separator))
	}
	for _, frag := range frozenDocsLines[rel] {
		if strings.Contains(line, frag) {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }
