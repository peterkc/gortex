package excludes

import (
	"path/filepath"
	"slices"
	"strings"
	"sync"

	ignore "github.com/sabhiram/go-gitignore"

	"github.com/zzet/gortex/internal/pathkey"
)

// Matcher tests whether a path should be excluded from indexing/watching.
// It is safe for concurrent reads after construction.
type Matcher struct {
	ign      *ignore.GitIgnore
	patterns []string
	// Required literals cheaply rule out simple positive patterns. Positive
	// patterns with uncertain syntax retain their original compiled matcher.
	requiredLiterals []string
	unfiltered       *ignore.GitIgnore
	// Positive patterns proven to cover a matched directory and all its
	// children, from the lines after the first usable negation; earlier
	// excludes cannot override it. Blank entries preserve line numbers in
	// patterns. Nil when no such exclude exists.
	subtreeExcludes *ignore.GitIgnore
	negations       []parsedNegation
	// Whether any anchored negation exists, so the watcher check can return
	// before matching when none can apply.
	hasAnchoredNegation bool

	// The same list without Builtin's agent-config root lines, compiled on
	// first use; see withoutBuiltinAgentRoots.
	agentRootsLifted     *Matcher
	agentRootsLiftedOnce sync.Once
}

// New compiles the given patterns into a Matcher. A nil/empty list is
// valid and will match nothing.
//
// Patterns are folded to Unicode NFC so a pattern naming a non-ASCII
// directory matches paths regardless of which Unicode form the
// filesystem walk produced — MatchRel folds the candidate path to the
// same form before testing it.
//
// Each pattern is also run through literalizePattern before it reaches
// go-gitignore, whose compiler splices pattern text straight into a Go
// regexp and escapes only "." and "?". Without that step a line as
// ordinary as "*$" compiles to a regexp that matches every path and
// excludes the entire repository (#624). The Matcher keeps the original
// text in patterns — the rewrite is a compiler detail and must never
// reach a user-visible surface.
func New(patterns []string) *Matcher {
	cleaned := make([]string, 0, len(patterns))
	compiled := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		p = pathkey.Normalize(p)
		cleaned = append(cleaned, p)
		compiled = append(compiled, literalizePattern(p))
	}
	m := &Matcher{
		ign:      ignore.CompileIgnoreLines(compiled...),
		patterns: cleaned,
	}
	var unfiltered []string
	subtreePatterns := make([]string, len(compiled))
	hasSubtreeExclude := false
	seen := make(map[string]bool)
	for i, p := range cleaned {
		if strings.HasPrefix(p, "!") {
			if negation, ok := parseNegation(p, i+1); ok {
				m.negations = append(m.negations, negation)
				m.hasAnchoredNegation = m.hasAnchoredNegation || negation.anchored
			}
			continue
		}
		// An exclude can only override negations that precede it.
		if len(m.negations) > 0 && coversSubtree(p) {
			subtreePatterns[i] = compiled[i]
			hasSubtreeExclude = true
		}
		literal := requiredLiteral(p)
		if literal == "" {
			unfiltered = append(unfiltered, compiled[i])
		} else if !seen[literal] {
			seen[literal] = true
			m.requiredLiterals = append(m.requiredLiterals, literal)
		}
	}
	if hasSubtreeExclude {
		m.subtreeExcludes = ignore.CompileIgnoreLines(subtreePatterns...)
	}
	if len(m.requiredLiterals) > 0 && len(unfiltered) > 0 {
		m.unfiltered = ignore.CompileIgnoreLines(unfiltered...)
	}
	return m
}

// coversSubtree identifies positive patterns whose directory matches extend
// to every child. Directory patterns retain the compiler's trailing /.*;
// ordinary globs retain its /.* suffix or a final star that consumes children.
// A terminal **/ loses its slash during compilation; /** can match only the
// directory slash (unless preceded by a literal path). Backslash escapes and
// non-directory classes can also match that slash alone. Keep those uncertain
// shapes conservative rather than treating every directory match as coverage.
func coversSubtree(pattern string) bool {
	if strings.Contains(pattern, `\`) || strings.Contains(pattern, "//") || strings.HasSuffix(pattern, "**/") {
		return false
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return prefix != "" && !strings.ContainsAny(prefix, "*?[")
	}
	return strings.HasSuffix(pattern, "/") || !strings.Contains(pattern, "[")
}

// requiredLiteral returns a substring every match of a simple glob must
// contain. Split at slashes too: a/b/* can match the directory a/b itself,
// and ** can consume zero path segments. Uncertain syntax stays unfiltered.
func requiredLiteral(pattern string) string {
	if strings.ContainsAny(pattern, "\\?[]") {
		return ""
	}
	var longest string
	for _, part := range strings.FieldsFunc(pattern, func(r rune) bool { return r == '*' || r == '/' }) {
		if len(part) > len(longest) {
			longest = part
		}
	}
	return longest
}

func (m *Matcher) couldMatch(rel string) bool {
	if len(m.requiredLiterals) == 0 {
		return true
	}
	for _, literal := range m.requiredLiterals {
		if strings.Contains(rel, literal) {
			return true
		}
	}
	return m.unfiltered != nil && m.unfiltered.MatchesPath(rel)
}

// Patterns returns the cleaned pattern list (empties and comments removed).
func (m *Matcher) Patterns() []string {
	if m == nil {
		return nil
	}
	out := make([]string, len(m.patterns))
	copy(out, m.patterns)
	return out
}

// cleanedBuiltin is Builtin as New cleans it.
var cleanedBuiltin = sync.OnceValue(func() []string { return New(Builtin).patterns })

// withoutBuiltinAgentRoots returns m without the Builtin lines that exclude
// the agent-config roots wholesale, compiled once. Effective exclude lists
// start with Builtin; a list that does not holds only the user's lines and
// is returned as is, and a copy of the line after Builtin stays.
func (m *Matcher) withoutBuiltinAgentRoots() *Matcher {
	m.agentRootsLiftedOnce.Do(func() {
		builtin := cleanedBuiltin()
		if len(m.patterns) < len(builtin) || !slices.Equal(m.patterns[:len(builtin)], builtin) {
			m.agentRootsLifted = m
			return
		}
		patterns := make([]string, 0, len(m.patterns))
		for i, p := range m.patterns {
			if i < len(builtin) && isAgentConfigRootPattern(p) {
				continue
			}
			patterns = append(patterns, p)
		}
		m.agentRootsLifted = New(patterns)
	})
	return m.agentRootsLifted
}

// MatchRel reports whether a repo-root-relative path is excluded.
// Path separators are normalised to forward slashes and the path is
// folded to Unicode NFC — matching how New normalised the patterns —
// before matching, so a non-ASCII path component compares equal to its
// pattern whether the OS supplied it decomposed (macOS NFD) or
// precomposed (Linux / git NFC).
func (m *Matcher) MatchRel(relPath string) bool {
	if m == nil || m.ign == nil {
		return false
	}
	rel := pathkey.Normalize(filepath.ToSlash(relPath))
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || rel == "." {
		return false
	}
	if !m.couldMatch(rel) {
		return false
	}
	return m.ign.MatchesPath(rel)
}

// Explain reports whether a repo-root-relative path is excluded and, when
// it is, which pattern excluded it — in the caller's original wording, not
// the rewritten form New compiles.
//
// This is what makes an over-broad ignore self-diagnosing: a repo that
// indexes zero files can name the one line responsible instead of leaving
// the operator to bisect a .gitignore by hand.
func (m *Matcher) Explain(relPath string) (bool, string) {
	if m == nil || m.ign == nil {
		return false, ""
	}
	rel := pathkey.Normalize(filepath.ToSlash(relPath))
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || rel == "." {
		return false, ""
	}
	matched, ip := m.ign.MatchesPathHow(rel)
	if !matched || ip == nil {
		return matched, ""
	}
	// LineNo is 1-based into the slice New handed the compiler, which is
	// index-aligned with m.patterns.
	if ip.LineNo >= 1 && ip.LineNo <= len(m.patterns) {
		return true, m.patterns[ip.LineNo-1]
	}
	return true, ip.Line
}

// MatchAbs reports whether an absolute path under root is excluded.
// Returns false if path is not under root.
func (m *Matcher) MatchAbs(absPath, root string) bool {
	return m.MatchAbsDir(absPath, root, false)
}

// MatchAbsDir reports whether an absolute path under root is excluded.
// When isDir is true the path is treated as a directory, so a pattern
// written with a trailing slash (e.g. "build/") matches the directory
// itself — letting the caller prune the whole subtree instead of
// descending it and re-testing every file. Returns false if path is
// not under root.
func (m *Matcher) MatchAbsDir(absPath, root string, isDir bool) bool {
	if m == nil || m.ign == nil {
		return false
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return false
	}
	if isDir {
		rel += "/"
	}
	return m.MatchRel(rel)
}

// ExplainAbsDir is MatchAbsDir plus the pattern that excluded the path,
// in the caller's original wording. The pattern is "" when nothing
// matched.
func (m *Matcher) ExplainAbsDir(absPath, root string, isDir bool) (bool, string) {
	if m == nil || m.ign == nil {
		return false, ""
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return false, ""
	}
	if isDir {
		rel += "/"
	}
	return m.Explain(rel)
}

// HasNegatedDescendant reports whether any re-include ("!") pattern in
// the matcher could re-include a path strictly beneath relDir. Negations
// overridden by a later exclusion covering the whole subtree do not count.
//
// The index walk prunes an excluded directory with filepath.SkipDir so
// it never descends a subtree it would only throw away. But go-gitignore
// treats "*" as matching across "/", so a blanket like "a/b/*" reports
// the directory "a/b" itself as excluded — pruning it would skip a later
// "!a/b/keep/" re-include before the walk ever reaches the child. This
// lets the walk ask "could a negation resurrect something under here?"
// and keep descending when the answer is yes, mirroring git, which never
// prunes a directory a negation could re-include a child from.
//
// relDir is a repo-root-relative, forward-slash directory path (a
// trailing slash and a leading "./" are tolerated). The check is
// deliberately conservative: an unanchored or wildcard-leading negation
// can match at varying depths, so it is treated as "could be under
// anything" and the directory is kept rather than pruned.
func (m *Matcher) HasNegatedDescendant(relDir string) bool {
	return m.hasNegatedDescendant(relDir, false)
}

// HasAnchoredNegatedDescendant counts only negations with a leading or
// internal slash and a literal first segment, honoring later subtree excludes.
func (m *Matcher) HasAnchoredNegatedDescendant(relDir string) bool {
	return m.hasNegatedDescendant(relDir, true)
}

func (m *Matcher) hasNegatedDescendant(relDir string, anchoredOnly bool) bool {
	if m == nil || len(m.negations) == 0 || anchoredOnly && !m.hasAnchoredNegation {
		return false
	}
	relDir = pathkey.Normalize(filepath.ToSlash(relDir))
	relDir = strings.TrimPrefix(relDir, "./")
	relDir = strings.TrimSuffix(relDir, "/")
	if relDir == "." {
		relDir = ""
	}
	lastExclude := 0
	if relDir != "" && m.subtreeExcludes != nil {
		// Only negations after the last covering exclude can resurrect a
		// descendant. Other positive patterns cannot prove subtree coverage.
		if matched, p := m.subtreeExcludes.MatchesPathHow(relDir + "/"); matched {
			lastExclude = p.LineNo
		}
	}
	for _, p := range m.negations {
		if p.lineNo <= lastExclude || anchoredOnly && (!p.anchored || p.unanchored) {
			continue
		}
		// An unknown anchor stays conservative in both modes; unanchored
		// and wildcard-leading negations stay conservative for walks only.
		if p.anchor == "" || relDir == "" {
			return true
		}
		// The negation's match-set intersects relDir's subtree when its
		// literal anchor sits at or under relDir, or relDir sits under the
		// anchor (a wildcard tail can then still reach into relDir).
		if p.anchor == relDir || hasDirPrefix(p.anchor, relDir) || hasDirPrefix(relDir, p.anchor) {
			return true
		}
	}
	return false
}

func hasDirPrefix(path, dir string) bool {
	return len(path) > len(dir) && path[len(dir)] == '/' && strings.HasPrefix(path, dir)
}

// parsedNegation holds the classification shared by the walk and watcher.
// lineNo is 1-based into patterns, just like go-gitignore's LineNo.
type parsedNegation struct {
	lineNo   int
	anchor   string
	anchored bool
	// No fixed anchor: only the walk counts these. An unsure escape also
	// has no anchor, but remains conservative in both modes instead.
	unanchored bool
}

func parseNegation(pattern string, lineNo int) (parsedNegation, bool) {
	np := strings.TrimSpace(pattern[1:])
	rooted := strings.HasPrefix(np, "/")
	np = strings.TrimSuffix(strings.TrimPrefix(np, "/"), "/")
	if np == "" {
		return parsedNegation{}, false
	}
	first, _, slash := strings.Cut(np, "/")
	literal, kind := literalSegment(first)
	p := parsedNegation{
		lineNo:   lineNo,
		anchored: (rooted || slash) && kind != segWildcard,
	}
	if !slash {
		if rooted && kind == segLiteral {
			p.anchor = literal
		} else {
			p.unanchored = !rooted || kind != segUnsure
		}
	} else if kind != segUnsure {
		p.anchor = literalAnchor(np)
		p.unanchored = p.anchor == ""
	}
	return p, true
}

// literalAnchor returns the leading path segments of a slash-bearing
// gitignore pattern up to (but excluding) the first segment that is not
// a plain literal (see literalSegment). It returns "" when the first
// segment is a wildcard ("*", "**", "?foo", ...). Escapes are decoded,
// so `foo\*/keep.go` anchors at `foo*`.
func literalAnchor(pattern string) string {
	segs := strings.Split(pattern, "/")
	lit := make([]string, 0, len(segs))
	for _, s := range segs {
		u, kind := literalSegment(s)
		if kind != segLiteral {
			break
		}
		lit = append(lit, u)
	}
	return strings.Join(lit, "/")
}

// segKind classifies one "/"-separated segment of a negation.
type segKind int

const (
	segLiteral  segKind = iota // plain text once escapes are decoded
	segWildcard                // holds an unescaped "*", "?" or bracket expression
	segUnsure                  // holds an escape the matcher may read differently
)

// literalEscapes are the characters that a backslash makes literal both
// in gitignore and in the compiled matcher: go-gitignore maps `\*` back
// to a star, and the rest reach the regexp as plain escapes. go-gitignore
// rewrites other escapes such as `\?`, `\.` or a letter, so the compiled
// matcher does not read them as gitignore does.
const literalEscapes = `*[\ ` + regexOnlyMeta

// literalSegment returns segment s with its escapes decoded and its kind.
// A trailing backslash escapes the slash the split removed, so it makes
// the segment unsure too.
func literalSegment(s string) (string, segKind) {
	if !strings.ContainsAny(s, `*?[\`) {
		return s, segLiteral
	}
	var b strings.Builder
	wild := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			if i+1 == len(s) || strings.IndexByte(literalEscapes, s[i+1]) < 0 {
				return "", segUnsure
			}
			i++
			b.WriteByte(s[i])
		case '*', '?':
			wild = true
		case '[':
			// literalizePattern keeps an unclosed "[" as a literal
			// character; only a bracket expression is a wildcard.
			if isBracketExpr(s[i:]) {
				wild = true
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
	if wild {
		return "", segWildcard
	}
	return b.String(), segLiteral
}

// isBracketExpr reports whether s starts with a bracket expression that
// the compiled matcher reads as a character class.
func isBracketExpr(s string) bool {
	_, _, ok := scanBracketExpr(s)
	return ok
}
