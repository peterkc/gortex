package indexer

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

const (
	rebuildNamespaceKeyLimit      = 4096
	rebuildNamespaceEntryLimit    = 256 << 10
	rebuildNamespaceKeyBytesLimit = 16 << 20
)

var (
	errRebuildResolverNamespace   = errors.New("unproved frozen resolver namespace")
	errRebuildResolverInputDenied = errors.New("frozen resolver input denied by admission")
)

type rebuildNamespaceKind uint8

const (
	rebuildNamespaceUnknown rebuildNamespaceKind = iota
	rebuildNamespaceRegular
	rebuildNamespaceDirectory
	rebuildNamespaceDenied
	rebuildNamespaceUnsupported // Symlink or special object. Never a followed-stat result.
	rebuildNamespaceClose       // Exhaustive, but not bound for stat-style probes.
	rebuildNamespaceCloseExact  // Exhaustive, with canonical byte-exact lookup (AC-54).
	rebuildNamespaceAbsent      // Lookup result only; never a row.
)

// Each row is one checked immediate child or a closure for a real directory.
// Keep the key's bytes unchanged on the wire. Only closure may use the root key ".".
type rebuildNamespaceRow struct {
	key  string
	kind rebuildNamespaceKind
}

type rebuildNamespaceEntry struct {
	kind  rebuildNamespaceKind
	close rebuildNamespaceKind
}

// The zero value accepts rows from one owner. Do not copy during construction.
// Retains at most EntryLimit child entries plus the root closure, and at most
// KeyBytesLimit bytes of copied child keys, apart from Go map/allocator overhead.
// The owner separately charges raw bytes and the sidecar's encoded bytes and rows.
type rebuildResolverNamespace struct {
	entries  map[string]rebuildNamespaceEntry
	keyBytes int
	invalid  error
	frozen   bool
}

// add consumes the same rows at capture and replay. Emit a directory's own row
// before its children, and all its immediate children before its closure. Only
// the enumerator can attest successful exhaustive enumeration of the same held
// identity, including opaque boundary entries. On drift or incomplete evidence,
// do not certify a closure. A failed capture or sidecar visit must be discarded.
func (n *rebuildResolverNamespace) add(row rebuildNamespaceRow) error {
	if n.frozen {
		return fmt.Errorf("%w: already frozen", errRebuildResolverNamespace)
	}
	if n.invalid == nil {
		n.invalid = n.addRow(row)
	}
	return n.invalid
}

func (n *rebuildResolverNamespace) addRow(row rebuildNamespaceRow) error {
	closing := row.kind == rebuildNamespaceClose || row.kind == rebuildNamespaceCloseExact
	if !validRebuildNamespaceKey(row.key) && (!closing || row.key != ".") {
		return fmt.Errorf("%w: nonportable row key", errRebuildResolverNamespace)
	}
	entry, seen := n.entries[row.key]
	if closing {
		if (row.key != "." && entry.kind != rebuildNamespaceDirectory) || entry.close != rebuildNamespaceUnknown {
			return fmt.Errorf("%w: closure without a checked open directory", errRebuildResolverNamespace)
		}
		n.put(row.key, rebuildNamespaceEntry{kind: rebuildNamespaceDirectory, close: row.kind})
		return nil
	}
	switch row.kind {
	case rebuildNamespaceRegular, rebuildNamespaceDirectory, rebuildNamespaceDenied, rebuildNamespaceUnsupported:
	default:
		return fmt.Errorf("%w: invalid entry kind %d", errRebuildResolverNamespace, row.kind)
	}
	parent := path.Dir(row.key)
	p := n.entries[parent]
	if seen || (parent != "." && p.kind != rebuildNamespaceDirectory) || p.close != rebuildNamespaceUnknown {
		return fmt.Errorf("%w: duplicate entry or entry without a checked open parent", errRebuildResolverNamespace)
	}
	count := len(n.entries)
	if _, rootClosed := n.entries["."]; rootClosed {
		count--
	}
	if count >= rebuildNamespaceEntryLimit || len(row.key) > rebuildNamespaceKeyBytesLimit-n.keyBytes {
		return fmt.Errorf("%w: retained namespace cap", errRebuildResolverNamespace)
	}
	n.put(row.key, rebuildNamespaceEntry{kind: row.kind})
	n.keyBytes += len(row.key)
	return nil
}

func (n *rebuildResolverNamespace) put(key string, entry rebuildNamespaceEntry) {
	if n.entries == nil {
		n.entries = make(map[string]rebuildNamespaceEntry)
	}
	// Even replacement may retain the supplied string's backing storage.
	n.entries[strings.Clone(key)] = entry
}

// Freeze only after successful capture or complete, digest-verified replay.
// An open directory is still a positive entry, but proves no absent children.
func (n *rebuildResolverNamespace) freeze() error {
	if n.invalid != nil {
		return n.invalid
	}
	n.frozen = true
	return nil
}

// lookup answers only exact name/type queries. Callers must separately justify
// projecting a result into a native WalkDir or follow-stat operation. Do not
// clean raw paths or pass a trailing-directory request as a plain key.
func (n *rebuildResolverNamespace) lookup(key string) (rebuildNamespaceKind, error) {
	if !n.frozen || !validRebuildNamespaceKey(key) {
		return rebuildNamespaceUnknown, fmt.Errorf("%w: unfrozen namespace or nonportable query %q", errRebuildResolverNamespace, key)
	}
	switch kind := n.entries[key].kind; kind {
	case rebuildNamespaceRegular, rebuildNamespaceDirectory:
		return kind, nil
	case rebuildNamespaceUnknown:
		// A closed exact immediate parent is the only negative authority.
		if n.entries[path.Dir(key)].close == rebuildNamespaceCloseExact {
			return rebuildNamespaceAbsent, nil
		}
	}
	// An opaque subtree carries only its boundary entry. An unlisted
	// descendant of one is denied, not an unknown admission decision.
	for ancestor := key; ; {
		if n.entries[ancestor].kind == rebuildNamespaceDenied {
			return rebuildNamespaceUnknown, fmt.Errorf("%w: lookup %q: %w", errRebuildResolverNamespace, key, errRebuildResolverInputDenied)
		}
		cut := strings.LastIndexByte(ancestor, '/')
		if cut < 0 {
			break
		}
		ancestor = ancestor[:cut]
	}
	return rebuildNamespaceUnknown, fmt.Errorf("%w: no checked answer for %q", errRebuildResolverNamespace, key)
}

// probeFile interprets a checked slash path with stat-style file semantics.
// A missing component in a byte-exact closed parent, or a regular ancestor,
// proves that the full path is not a file. Unknown or opaque paths refuse.
func (n *rebuildResolverNamespace) probeFile(key string) (bool, error) {
	if n != nil && n.frozen && n.entries["."].close != rebuildNamespaceUnknown && key == "." {
		return false, nil // the held checkout root is a directory
	}
	if n == nil || !n.frozen || !validRebuildNamespaceKey(key) {
		return false, fmt.Errorf("%w: invalid frozen file probe %q", errRebuildResolverNamespace, key)
	}
	for start := 0; ; {
		end := len(key)
		if i := strings.IndexByte(key[start:], '/'); i >= 0 {
			end = start + i
		}
		kind, err := n.lookup(key[:end])
		if err != nil {
			return false, fmt.Errorf("file probe %q: %w", key, err)
		}
		if end == len(key) {
			return kind == rebuildNamespaceRegular, nil
		}
		if kind != rebuildNamespaceDirectory {
			return false, nil
		}
		start = end + 1
	}
}

// rebuildLinkCandidateProbe projects only candidate-file presence. Keep the
// original namespace for independent config, manifest and directory lookups.
func rebuildLinkCandidateProbe(n *rebuildResolverNamespace, census *rebuildExtractorCensus,
	classifications map[string]rebuildClassificationFact,
) func(string) (bool, error) {
	return func(key string) (bool, error) {
		present, err := n.probeFile(key)
		if err != nil || !present {
			return present, err
		}
		if census == nil || uint64(len(census.byPath)) != census.admitted {
			return false, fmt.Errorf("%w: incomplete extractor census", errRebuildCallerAttribution)
		}
		if _, admitted := census.byPath[key]; admitted {
			heldOut, err := census.holdsOutLinkInput(key)
			return !heldOut && err == nil, err
		}
		// A nonparse key can still have independently checked regular-file
		// evidence. A validated deferred skip, however, is a held-out input.
		return !rebuildHoldsOutCheckedSkip(classifications[key]), nil
	}
}

func validRebuildNamespaceKey(key string) bool {
	return len(key) > 0 && len(key) <= rebuildNamespaceKeyLimit && key != "." && key != ".." &&
		!path.IsAbs(key) && !strings.HasPrefix(key, "../") && !strings.ContainsAny(key, "\\:\x00") && path.Clean(key) == key
}
