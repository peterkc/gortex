package excludes

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/pathkey"
)

// NewWithIncludes compiles force-includes separately from gitignore rules.
// Anchored literals open their ancestors; basename literals need open parents.
func NewWithIncludes(patterns, includes []string) *Matcher {
	m := New(patterns)
	var globs []string
	for _, p := range includes {
		p = pathkey.Normalize(filepath.ToSlash(strings.TrimSpace(p)))
		p = strings.TrimPrefix(p, "!")
		p = strings.TrimPrefix(p, "./")
		if p == "" {
			continue
		}
		anchored := strings.HasPrefix(p, "/") || strings.Contains(strings.TrimSuffix(p, "/"), "/")
		if !anchored || strings.ContainsAny(p, `*?[\`) {
			if anchored && !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			globs = append(globs, p)
		} else {
			m.literalIncludes = append(m.literalIncludes, strings.TrimPrefix(p, "/"))
		}
	}
	if len(globs) > 0 {
		m.includes = New(globs)
	}
	return m
}

// ParentLimitedIncludes returns includes that cannot reopen their ancestors.
func (m *Matcher) ParentLimitedIncludes() []string {
	if m == nil {
		return nil
	}
	return m.includes.Patterns()
}

// IncludesRel reports a force-include. Basename literals reopen their own
// target, but only anchored literals can open excluded ancestors. The caller still
// enforces hard exclusions and applies per-directory ignore files independently.
func (m *Matcher) IncludesRel(rel string) bool {
	if m == nil || (m.includes == nil && len(m.literalIncludes) == 0) {
		return false
	}
	rel = pathkey.Normalize(filepath.ToSlash(rel))
	rel = strings.TrimPrefix(rel, "./")
	if m.includesLiteral(rel) || m.includesBasename(rel) {
		return true
	}
	if strings.HasSuffix(rel, "/") || m.includes == nil || !m.includes.MatchRel(rel) {
		return false
	}
	return !m.excludesParent(rel)
}

func (m *Matcher) includesBasename(rel string) bool {
	if m.includes == nil {
		return false
	}
	for _, p := range m.includes.patterns {
		target := strings.TrimSuffix(p, "/")
		if strings.ContainsAny(target, `/*?[\`) {
			continue
		}
		parent, body := "", rel
		for body != "" {
			segment, rest, slash := strings.Cut(body, "/")
			if segment == target && (!strings.HasSuffix(p, "/") || slash) && !m.excludesParent(parent) {
				return true
			}
			if !slash {
				break
			}
			parent += segment + "/"
			body = rest
		}
	}
	return false
}

func (m *Matcher) includesLiteral(rel string) bool {
	isDir := strings.HasSuffix(rel, "/")
	node := strings.TrimSuffix(rel, "/")
	for _, p := range m.literalIncludes {
		target := strings.TrimSuffix(p, "/")
		if node == target || strings.HasPrefix(node, target+"/") || (isDir && strings.HasPrefix(target, node+"/")) {
			return true
		}
	}
	return false
}

func (m *Matcher) excludesParent(rel string) bool {
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' && !m.includesLiteral(rel[:i]+"/") && m.dirs.MatchesPath(rel[:i]) {
			return true
		}
	}
	return false
}
