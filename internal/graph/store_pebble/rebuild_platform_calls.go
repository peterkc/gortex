package store_pebble

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

type rebuildPlatformReader interface {
	pebble.Reader
	NewIterWithContext(context.Context, *pebble.IterOptions) (*pebble.Iterator, error)
}

// SettleRebuildPlatformCalls adds final status to Go builtin Calls after all
// private linker turns. Web Calls classify only after ordinary lookup in main.
func (b *BuildStore) SettleRebuildPlatformCalls(ctx context.Context, extractorOf func(string) string) error {
	const revision = graph.PlatformCatalogRevision
	view := b.RebuildView()
	return view.visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("platform call identity does not match key")
		}
		entry, exact := graph.LookupPlatformBuiltin(revision, e.To)
		if exact && entry.Language == "go" {
			want, err := b.rebuildGoBuiltinCallStatus(ctx, snap, e, entry, extractorOf)
			if err != nil {
				return err
			}
			if raw, exists := e.Meta[graph.MetaLinkStatus]; exists {
				if raw != string(want) {
					return invalid("incorrect stored Go builtin call status")
				}
			} else {
				if e.Meta == nil {
					e.Meta = make(map[string]any, 1)
				}
				e.Meta[graph.MetaLinkStatus] = string(want)
				return b.withWrites(func() error { return b.refreshPendingEdge(e) })
			}
		}
		return b.validateRebuildPlatformEdge(ctx, snap, e, revision, extractorOf)
	})
}

// ValidateRebuildCallDelta runs the same builtin and status checks for a
// retained Calls delta against the private working graph before its offer.
// It leaves rows and custody unchanged.
func (b *BuildStore) ValidateRebuildCallDelta(ctx context.Context, edge *graph.Edge, revision uint32, extractorOf func(string) string) error {
	if edge == nil || edge.Kind != graph.EdgeCalls {
		return invalid("expected a retained Call")
	}
	return b.RebuildView().read(ctx, func(snap *pebble.Snapshot) error {
		if err := b.validateRebuildPlatformEdge(ctx, snap, edge, revision, extractorOf); err != nil {
			return err
		}
		for i, id := range [...]string{edge.From, edge.To} {
			if i == 1 && (graph.IsBuiltinStub(id) || (graph.IsUnresolvedTarget(id) && graph.UnresolvedName(id) != "")) {
				continue
			}
			n, err := pendingGet(snap, pendingNodeKey(id), decodeNodeLight)
			if err != nil {
				return err
			}
			if n == nil || n.ID != id || n.Kind == graph.KindBuiltin || graph.IsProxyNode(n) {
				return fmt.Errorf("%w: invalid retained Call endpoint %q", errInvalidRecord, id)
			}
		}
		return nil
	})
}

func (b *BuildStore) validateRebuildPlatformEdge(ctx context.Context, reader rebuildPlatformReader, e *graph.Edge, revision uint32, extractorOf func(string) string) error {
	if graph.IsBuiltinStub(e.From) {
		if _, err := b.provedBuiltinSourceOwner(e); err != nil {
			return err
		}
		if extractorOf == nil || extractorOf(e.FilePath) != "go" {
			return invalid("builtin source is not a recorded Go site")
		}
	}
	builtin := graph.IsBuiltinStub(e.To)
	if e.Kind != graph.EdgeCalls || !builtin {
		if _, stored := e.Meta[graph.MetaLinkStatus]; stored {
			return invalid("link_status outside a builtin Call")
		}
		if !builtin {
			return nil
		}
	}
	entry, exact := graph.LookupPlatformBuiltin(revision, e.To)
	if exact && entry.Language == "go" {
		if e.Kind != graph.EdgeCalls {
			if err := b.checkGoBuiltinTargetScope(reader, e, entry, extractorOf); err != nil {
				return err
			}
			switch e.Kind {
			case graph.EdgeReferences, graph.EdgeReads, graph.EdgeArgOf, graph.EdgeValueFlow,
				graph.EdgeReturnsTo, graph.EdgeTypedAs, graph.EdgeReturns, graph.EdgeInstantiates,
				graph.EdgeCaptures, graph.EdgeThrows:
				return nil
			default:
				return invalid("unsupported Go builtin target role")
			}
		}
		want, err := b.rebuildGoBuiltinCallStatus(ctx, reader, e, entry, extractorOf)
		if err != nil {
			return err
		}
		got, ok := e.LinkStatus()
		if !ok || got != want {
			return invalid("incorrect stored Go builtin call status")
		}
		return nil
	}
	return checkRebuildWebBuiltinTarget(reader, e, revision, extractorOf)
}

func (b *BuildStore) checkGoBuiltinTargetScope(reader rebuildPlatformReader, e *graph.Edge, entry graph.PlatformBuiltin, extractorOf func(string) string) error {
	if extractorOf == nil || extractorOf(e.FilePath) != "go" {
		return invalid("Go builtin target is not a recorded Go site")
	}
	file, err := pendingGet(reader, pendingNodeKey(e.FilePath), decodeNodeLight)
	if err != nil {
		return err
	}
	if file == nil || file.ID != e.FilePath || file.FilePath != e.FilePath || file.Kind != graph.KindFile ||
		file.Language != "go" || entry.ID(file.RepoPrefix) != e.To {
		return invalid("Go builtin target recording scope differs")
	}
	if !graph.IsBuiltinStub(e.From) {
		owner, err := pendingGet(reader, pendingNodeKey(e.From), decodeNodeLight)
		if err != nil {
			return err
		}
		if owner != nil && owner.RepoPrefix != file.RepoPrefix {
			return invalid("Go builtin target caller scope differs")
		}
	}
	return nil
}

func (b *BuildStore) rebuildGoBuiltinCallStatus(ctx context.Context, reader rebuildPlatformReader, e *graph.Edge, entry graph.PlatformBuiltin, extractorOf func(string) string) (graph.LinkStatus, error) {
	if entry.Category == "const" {
		return "", invalid("Go builtin constant cannot be called")
	}
	if err := b.checkGoBuiltinTargetScope(reader, e, entry, extractorOf); err != nil {
		return "", err
	}
	hint, ok := e.BuiltinReceiverHint()
	if entry.Category != "" || !ok || hint.Language != "go" || hint.Form != "unbound" || hint.Name != entry.Name {
		return graph.LinkStatusProbablePlatform, nil
	}
	// A same-directory declaration in any Go package blocks exactness. This
	// also handles external test packages conservatively without assuming a
	// package name from the filename. Graph candidates use checked postings.
	repo, dir := graph.StubRepoPrefix(e.To), path.Dir(e.FilePath)
	blocked := false
	prefix := keyStrings([]byte{familyName, nameRebuildScoped}, entry.Name, repo, "go")
	err := pendingVisit(ctx, reader, prefix, func(r row) error {
		_, _, id, _, err := readRebuildCandidateIndex(nameRebuildScoped, r.key, r.value)
		if err != nil {
			return err
		}
		n, err := pendingGet(reader, pendingNodeKey(id), decodeNodeLight)
		if err != nil {
			return err
		}
		if n == nil || n.ID != id || !bytes.Equal(r.key, rebuildCandidateKey(nameRebuildScoped, n)) {
			return invalid("stale Go builtin package candidate index entry")
		}
		if n.FilePath != "" && path.Dir(n.FilePath) == dir {
			switch n.Kind {
			case graph.KindFunction, graph.KindType, graph.KindInterface, graph.KindConstant, graph.KindVariable:
				blocked = true
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if blocked {
		return graph.LinkStatusProbablePlatform, nil
	}
	return graph.LinkStatusPlatform, nil
}

func checkRebuildWebBuiltinTarget(reader rebuildPlatformReader, e *graph.Edge, revision uint32, extractorOf func(string) string) error {
	if extractorOf == nil {
		return invalid("builtin target has no recording extractor")
	}
	var language string
	switch extractorOf(e.FilePath) {
	case "javascript":
		language = "js"
	case "typescript":
		language = "ts"
	case "python":
		if e.Kind != graph.EdgeCalls {
			language = "py"
		}
	}
	if language == "" {
		return invalid("builtin target recording language is not admitted")
	}
	tail := graph.StubRest(e.To)
	lang, tail, ok := strings.Cut(tail, "::")
	if !ok || lang != language {
		return invalid("builtin target language differs from its recording extractor")
	}
	_, name, ok := strings.Cut(tail, "::")
	if !ok {
		return invalid("malformed builtin target")
	}
	source, err := pendingGet(reader, pendingNodeKey(e.From), decodeNodeLight)
	if err != nil {
		return err
	}
	if source == nil || source.ID != e.From || source.Kind == graph.KindBuiltin {
		return invalid("builtin target has no ordinary source owner")
	}
	candidate, ok := graph.LookupPlatformCandidate(revision, language, name)
	if !ok {
		return invalid("unknown builtin target name")
	}
	switch e.Kind {
	case graph.EdgeReads, graph.EdgeReferences, graph.EdgeWrites, graph.EdgeArgOf, graph.EdgeValueFlow:
		if e.To == candidate.ID(source.RepoPrefix) {
			return nil // Keep the existing non-Call legacy classification.
		}
	case graph.EdgeCalls:
		want := graph.LinkStatusProbablePlatform
		if hint, ok := e.BuiltinReceiverHint(); ok {
			switch hint.Form {
			case "import", "shadowed", "user":
				return invalid("builtin call has contrary receiver evidence")
			}
			if exact, ok := graph.LookupPlatformMember(revision, hint, name); ok && exact.Language == language {
				candidate, want = exact, graph.LinkStatusPlatform
			}
		}
		if got, ok := e.LinkStatus(); ok && got == want && e.To == candidate.ID(source.RepoPrefix) {
			return nil
		}
	}
	return invalid("builtin target role, scope or status differs")
}
