package tsalias

import (
	"cmp"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// MaxFrozenAliasConfigBytes bounds raw bytes parsed into one config row.
// The caller bounds aggregate reads and encoded rows.
const MaxFrozenAliasConfigBytes = 1 << 20

var (
	ErrFrozenAliasConfigLimit = errors.New("frozen TS alias config exceeds byte limit")
	ErrFrozenAliasPath        = errors.New("nonportable frozen TS alias path")
	ErrFrozenAliasUnproved    = errors.New("unproved frozen TS alias input")
)

// FrozenAliasConfig is the normalized payload for one admitted config path.
// The lookup must return rows in the order produced by ParseFrozenAliasConfig.
// The caller owns the key and its admission proof. Rows are read-only in use.
type FrozenAliasConfig struct {
	BaseURL string
	Entries []Alias
}

// ParseFrozenAliasConfig reduces verified bytes to one ordered config payload.
// A nil row with no error is a native no-scope result (e.g. malformed JSONC
// or no baseUrl/paths), not a claim that the file was absent. No extends are read.
func ParseFrozenAliasConfig(data []byte) (*FrozenAliasConfig, error) {
	if len(data) > MaxFrozenAliasConfigBytes {
		return nil, ErrFrozenAliasConfigLimit
	}
	m := parseConfig(data, "", nil)
	if m == nil {
		return nil, nil
	}
	// Native can select an empty scope and thereby shadow other configs.
	// Its baseUrl cannot affect a target in that case.
	if len(m.Entries) == 0 {
		return &FrozenAliasConfig{}, nil
	}
	if !frozenAliasRelativeFragment(m.BaseURL) {
		return nil, fmt.Errorf("%w: baseUrl", ErrFrozenAliasPath)
	}
	if m.BaseURL != "" {
		m.BaseURL = path.Clean(m.BaseURL)
	}
	for _, a := range m.Entries {
		for _, t := range a.Targets {
			// Do not path-clean pieces of an unexpanded wildcard. Cleaning
			// happens in Resolve after substitution and joining the scope.
			if !frozenAliasRelativeFragment(t.Prefix) || strings.ContainsAny(t.Suffix, "\\\x00") {
				return nil, fmt.Errorf("%w: target template", ErrFrozenAliasPath)
			}
		}
	}
	// Native ranges the paths map before a stable prefix-length sort. Only
	// equal ranks change here: compare the full, losslessly recovered keys.
	slices.SortFunc(m.Entries, func(a, b Alias) int {
		if rank := cmp.Compare(len(b.AliasPrefix), len(a.AliasPrefix)); rank != 0 {
			return rank
		}
		return strings.Compare(frozenAliasPattern(a), frozenAliasPattern(b))
	})
	return &FrozenAliasConfig{BaseURL: m.BaseURL, Entries: m.Entries}, nil
}

func frozenAliasPattern(a Alias) string {
	if a.HasWildcard {
		return a.AliasPrefix + "*" + a.AliasSuffix
	}
	return a.AliasPrefix
}

// FrozenAliasStem is a slash-relative, extension-stripped target. The empty
// string with no error is an authoritative no-alias answer. A nonempty stem
// need not exist: native returns the first target when all its probes miss.
type FrozenAliasStem string

// ResolveFrozenAlias expands a bare specifier for a checkout-relative caller.
// config returns a normalized row or a proved no-scope (nil, nil) for each
// consulted path. probe returns false, nil only for a proved non-file. Both
// callbacks must return errors for unproved, ignored/non-admitted or outside
// paths, rather than fabricating a miss. These callbacks are the only inputs
// besides the caller and specifier; this function does not read the checkout.
func ResolveFrozenAlias(callerRel, specifier string, config func(rel string) (*FrozenAliasConfig, error), probe func(rel string) (bool, error)) (FrozenAliasStem, error) {
	if specifier == "" {
		return "", nil
	}
	if callerRel == "." || !frozenAliasPath(callerRel) {
		return "", fmt.Errorf("%w: caller", ErrFrozenAliasPath)
	}
	if config == nil {
		return "", fmt.Errorf("%w: config lookup not supplied", ErrFrozenAliasUnproved)
	}
	for dir := path.Dir(callerRel); ; dir = path.Dir(dir) {
		if !underSkippedDir(path.Join(dir, "jsconfig.json")) {
			// Load's WalkDir is lexical. NewCollection keeps the arrival
			// order for scopes at the same depth in the same directory.
			// LoadTree instead takes its arrival order from Tree.Files.
			for _, name := range [...]string{"jsconfig.json", "tsconfig.json"} {
				rel := path.Join(dir, name)
				cfg, err := config(rel)
				if err != nil {
					return "", fmt.Errorf("frozen TS alias config %q: %w", rel, err)
				}
				if cfg != nil {
					return resolveFrozenAliasInScope(dir, cfg, specifier, probe)
				}
			}
		}
		if dir == "." {
			return "", nil
		}
	}
}

func resolveFrozenAliasInScope(dir string, cfg *FrozenAliasConfig, specifier string, probe func(rel string) (bool, error)) (FrozenAliasStem, error) {
	if dir == "." {
		dir = ""
	}
	m := Map{DirPrefix: dir, BaseURL: cfg.BaseURL, Entries: cfg.Entries}
	var probeErr error
	m.exists = func(rel string) bool {
		var found bool
		switch {
		case !frozenAliasPath(rel):
			probeErr = ErrFrozenAliasPath
			return true
		case probe == nil:
			probeErr = ErrFrozenAliasUnproved
		default:
			found, probeErr = probe(rel)
		}
		if probeErr != nil {
			probeErr = fmt.Errorf("frozen TS alias probe %q: %w", rel, probeErr)
		}
		// Stop native Resolve's loop immediately on the first error as
		// well as on a hit. Discard the stem on error below.
		return found || probeErr != nil
	}
	stem := Resolve(&m, specifier)
	if probeErr != nil {
		return "", probeErr
	}
	// Stripping an extension can expose a parent segment (e.g. ...ts).
	if stem != "" && !frozenAliasPath(strings.TrimSuffix(stem, "/")) {
		return "", fmt.Errorf("%w: resolved stem", ErrFrozenAliasPath)
	}
	return FrozenAliasStem(stem), nil
}

func frozenAliasRelativeFragment(s string) bool {
	return !path.IsAbs(s) && !strings.ContainsAny(s, "\\\x00") && (len(s) < 2 || s[1] != ':')
}

func frozenAliasPath(s string) bool {
	return s != "" && s == path.Clean(s) && s != ".." && !strings.HasPrefix(s, "../") && frozenAliasRelativeFragment(s)
}
