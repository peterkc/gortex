package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.OverlayLayerContractProjectionReader     = (*Snapshot)(nil)
	_ graph.OverlayLayerContractRepoProjectionReader = (*Snapshot)(nil)
	_ graph.ContractFileProjectionReader             = (*Snapshot)(nil)
	_ graph.ContractRepoProjectionReader             = (*Snapshot)(nil)
	_ graph.ContractOwnerScalarLiveness              = (*Snapshot)(nil)
)

var contractOwnerKinds = [...]graph.EdgeKind{graph.EdgeProvides, graph.EdgeConsumes, graph.EdgeHandlesRoute}

// The cold Contracts stage materializes the selected scalar rows. It cannot
// retain a stale scalar from an older owner frontier: there are no edit deltas
// at this boundary. Keep native live scalars even if a route edge points to one.
func (s *Snapshot) ContractOwnerScalarLivenessGuaranteed() bool { return true }

func (s *Snapshot) LoadContractFileProjectionContext(ctx context.Context, repo string, paths []string) (graph.ContractFileProjection, error) {
	return graph.CompleteContractFileProjection(ctx, s, repo, paths)
}

func (s *Snapshot) LoadContractIDProjectionContext(ctx context.Context, ids []string) (graph.ContractFileProjection, error) {
	return graph.CompleteContractIDProjection(ctx, s, ids)
}

func (s *Snapshot) LoadContractRepoProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error) {
	return graph.CompleteContractRepoProjection(ctx, s, repo)
}

func (s *Snapshot) readContractProjection(ctx context.Context, read func(context.Context, *graph.ContractFileProjection, *int) error) (graph.ContractFileProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := s.contractReadWitness(ctx); err != nil {
		return graph.ContractFileProjection{}, err
	}
	p := graph.ContractFileProjection{
		FileNodes:   make(map[string][]*graph.Node),
		Targets:     make(map[string]*graph.Node),
		SourceNodes: make(map[string]*graph.Node),
	}
	budget := graph.ContractProjectionRowLimit
	if err := read(ctx, &p, &budget); err != nil {
		s.retainContractRead(err)
		return graph.ContractFileProjection{}, err
	}
	if err := errors.Join(ctx.Err(), s.ReadError()); err != nil {
		return graph.ContractFileProjection{}, err
	}
	return p, nil
}

func chargeContractProjection(budget *int) error {
	if *budget == 0 {
		return graph.ErrContractProjectionLimit
	}
	*budget--
	return nil
}

// Owner attribution needs only the stored source identity, not its shape.
func (s *Snapshot) contractOwnerSource(ctx context.Context, id string) (*graph.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n, err := point(s, pendingNodeKey(id), decodeNodeLight)
	if err == nil && n != nil && n.ID != id {
		return nil, invalid("contract projection node differs from its key")
	}
	return n, err
}

// Native file projection selects a canonical graph path, not the source
// node's repo. Enumerate exact repo prefixes for the F seeks, and use the
// edge's recording path for ownership even when the source is in another file.
func (s *Snapshot) LayerContractFileProjectionContext(ctx context.Context, _ string, paths []string) (graph.ContractFileProjection, error) {
	return s.readContractProjection(ctx, func(ctx context.Context, p *graph.ContractFileProjection, budget *int) error {
		if len(paths) == 0 {
			return nil
		}
		ordered := slices.Clone(paths)
		slices.Sort(ordered)
		ordered = slices.Compact(ordered)
		prefix := countKey("repo")
		err := scan(ctx, s, prefix, func(key, _ []byte) (bool, error) {
			repo, tail, err := readKeyString(key[len(prefix):])
			if err != nil || len(tail) != 0 {
				return false, invalid("contract projection repository counter key")
			}
			for _, path := range ordered {
				if err := s.contractFileNodes(ctx, p, budget, repo, path); err != nil {
					return false, err
				}
			}
			return true, nil
		})
		if err != nil {
			return err
		}
		return s.visitContractOwnerEdges(ctx, func(e *graph.Edge) error {
			if _, found := slices.BinarySearch(ordered, e.FilePath); !found {
				return nil
			}
			return s.addContractOwnerRow(ctx, &p.OwnerRows, budget, e)
		})
	})
}

func (s *Snapshot) contractFileNodes(ctx context.Context, p *graph.ContractFileProjection, budget *int, repo, path string) error {
	return scan(ctx, s, keyStrings([]byte{familyFile}, repo, path), func(key, raw []byte) (bool, error) {
		n, err := s.projectedContractNode(ctx, string(raw))
		if err != nil {
			return false, err
		}
		if n == nil || !bytes.Equal(key, keyStrings([]byte{familyFile}, n.RepoPrefix, n.FilePath, n.ID)) {
			return false, invalid("contract projection file index differs from its node")
		}
		if err := chargeContractProjection(budget); err != nil {
			return false, err
		}
		p.FileNodes[path] = append(p.FileNodes[path], n)
		if n.Kind == graph.KindContract {
			p.ScalarNodes = append(p.ScalarNodes, n)
		}
		return true, nil
	})
}

func (s *Snapshot) LayerContractIDProjectionContext(ctx context.Context, ids []string) (graph.ContractFileProjection, error) {
	return s.readContractProjection(ctx, func(ctx context.Context, p *graph.ContractFileProjection, budget *int) error {
		for id := range uniqueNonempty(ids) {
			n, err := s.projectedContractNode(ctx, id)
			if err != nil {
				return err
			}
			if n != nil {
				if err := chargeContractProjection(budget); err != nil {
					return err
				}
				p.SourceNodes[id] = n
				if n.Kind == graph.KindContract {
					p.Targets[id] = n
				}
			}
			for _, kind := range contractOwnerKinds {
				if err := s.contractAdjacentOwners(ctx, &p.OwnerRows, budget, id, kind, edgeIn); err != nil {
					return err
				}
				if err := s.contractAdjacentOwners(ctx, &p.OutgoingOwnerRows, budget, id, kind, edgeOutKind); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Snapshot) contractAdjacentOwners(ctx context.Context, rows *[]graph.RepoEdgeRow, budget *int, id string, kind graph.EdgeKind, dir byte) error {
	return scan(ctx, s, keyStrings([]byte{familyEdge, dir}, id, string(kind)), func(key, raw []byte) (bool, error) {
		e, err := decodeEdge(raw)
		if err != nil {
			return false, err
		}
		identity := edgeIdentity(e)
		want := keyStrings([]byte{familyEdge, dir}, e.To, string(e.Kind))
		if dir == edgeOutKind {
			want = appendKeyLine(keyStrings([]byte{familyEdge, dir}, e.From, string(e.Kind)), e.Line)
		}
		if !bytes.Equal(key, append(want, identity[:]...)) {
			return false, invalid("contract projection adjacency differs from its edge")
		}
		return true, s.addContractOwnerRow(ctx, rows, budget, e)
	})
}

func (s *Snapshot) LayerContractRepoProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error) {
	return s.readContractProjection(ctx, func(ctx context.Context, p *graph.ContractFileProjection, budget *int) error {
		err := scan(ctx, s, keyStrings([]byte{familyNodeKind}, string(graph.KindContract)), func(key, raw []byte) (bool, error) {
			n, err := s.projectedContractNode(ctx, string(raw))
			if err != nil {
				return false, err
			}
			if n == nil || n.Kind != graph.KindContract || !bytes.Equal(key, keyStrings([]byte{familyNodeKind}, string(n.Kind), n.ID)) {
				return false, invalid("contract projection kind index differs from its node")
			}
			if n.RepoPrefix == repo {
				if err := chargeContractProjection(budget); err != nil {
					return false, err
				}
				p.ScalarNodes = append(p.ScalarNodes, n)
			}
			return true, nil
		})
		if err != nil {
			return err
		}
		return s.visitContractOwnerEdges(ctx, func(e *graph.Edge) error {
			source, err := s.contractOwnerSource(ctx, e.From)
			if err != nil {
				return err
			}
			// SQLite seeds use the saved source repo, then filter explicit
			// owner scope. Missing sources go to checked completion.
			if source != nil && source.RepoPrefix != repo {
				return nil
			}
			var sourceRepo string
			if source != nil {
				sourceRepo = source.RepoPrefix
			}
			if explicit, ok := e.Meta["contract_owner_repo_prefix"].(string); ok {
				if explicit != repo {
					return nil
				}
				sourceRepo = explicit
			}
			if err := chargeContractProjection(budget); err != nil {
				return err
			}
			p.OwnerRows = append(p.OwnerRows, graph.RepoEdgeRow{RepoPrefix: sourceRepo, Edge: e})
			return nil
		})
	})
}

func (s *Snapshot) addContractOwnerRow(ctx context.Context, rows *[]graph.RepoEdgeRow, budget *int, e *graph.Edge) error {
	if err := chargeContractProjection(budget); err != nil {
		return err
	}
	source, err := s.contractOwnerSource(ctx, e.From)
	if err != nil {
		return err
	}
	var repo string
	if source != nil {
		repo = source.RepoPrefix
	}
	*rows = append(*rows, graph.RepoEdgeRow{RepoPrefix: repo, Edge: e})
	return nil
}

func (s *Snapshot) visitContractOwnerEdges(ctx context.Context, visit func(*graph.Edge) error) error {
	for _, kind := range contractOwnerKinds {
		err := scan(ctx, s, keyStrings([]byte{familyEdge, edgeKind}, string(kind)), func(key, raw []byte) (bool, error) {
			e, err := point(s, raw, decodeEdge)
			if err != nil {
				return false, err
			}
			if e == nil || e.Kind != kind {
				return false, invalid("contract projection kind index missing its edge")
			}
			id := edgeIdentity(e)
			want := append(appendKeyLine(keyStrings([]byte{familyEdge, edgeKind}, string(kind), e.From), e.Line), id[:]...)
			if !bytes.Equal(raw, outEdgeKey(e, id)) || !bytes.Equal(key, want) {
				return false, invalid("contract projection kind index differs from its edge")
			}
			return true, visit(e)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
