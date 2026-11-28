package store_pebble

import (
	"context"
	"errors"
	"slices"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

func (s *Snapshot) contractReadWitness(ctx context.Context) (Witness, error) {
	if err := ctx.Err(); err != nil {
		return Witness{}, err
	}
	if err := s.ReadError(); err != nil {
		return Witness{}, err
	}
	w, err := s.Publication()
	if err != nil {
		if !errors.Is(err, ErrNotPublished) {
			s.retain(err)
		}
		return Witness{}, err
	}
	if w.Contracts == nil {
		return Witness{}, &ErrStagePending{Need: StageContracts, Published: w.Stage}
	}
	return w, nil
}

// ContractCoverageContext reports partial coverage for a positive answer.
// Set requireComplete for absence and count answers; a matching omission
// then returns ErrContractCoverageIncomplete, not a usable answer.
func (s *Snapshot) ContractCoverageContext(ctx context.Context, scope contracts.ReadScope, requireComplete bool) (bool, error) {
	w, err := s.contractReadWitness(ctx)
	if err != nil {
		return false, err
	}
	if w.Contracts.OmittedRecords == 0 {
		return false, ctx.Err()
	}
	partial := false
	visit := func(key, value []byte) (bool, error) {
		o, err := decodeContractOmission(key, value)
		if err != nil {
			return false, err
		}
		partial = (len(scope.FilePaths) == 0 || slices.Contains(scope.FilePaths, o.FilePath)) &&
			(len(scope.Types) == 0 || slices.Contains(scope.Types, o.Type)) &&
			(len(scope.ContractIDs) == 0 || o.ID == "" || slices.Contains(scope.ContractIDs, o.ID))
		return !partial, nil
	}
	if len(scope.RepoPrefixes) == 0 {
		err = scan(ctx, s, []byte{familyOmission}, visit)
	} else {
		for _, repo := range scope.RepoPrefixes {
			err = scan(ctx, s, keyStrings([]byte{familyOmission}, repo), visit)
			if err != nil || partial {
				break
			}
		}
	}
	if err != nil {
		s.retainContractRead(err)
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := s.ReadError(); err != nil {
		return false, err
	}
	if partial && requireComplete {
		return true, contracts.ErrContractCoverageIncomplete
	}
	return partial, nil
}

// retainContractRead keeps storage errors sticky, without poisoning a pinned
// snapshot on canceled or over-limit requests.
func (s *Snapshot) retainContractRead(err error) {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, graph.ErrContractProjectionLimit) {
		s.retain(err)
	}
}

// ContractShapesContext reads exact type IDs; a checked miss is absent from
// the map. The caller applies contract coverage to the enclosing answer.
func (s *Snapshot) ContractShapesContext(ctx context.Context, ids []string) (map[string]*contracts.Shape, error) {
	if _, err := s.contractReadWitness(ctx); err != nil {
		return nil, err
	}
	return s.contractShapes(ctx, ids)
}

func (s *Snapshot) contractShapes(ctx context.Context, ids []string) (map[string]*contracts.Shape, error) {
	if len(ids) > graph.ContractProjectionRowLimit {
		return nil, graph.ErrContractProjectionLimit
	}
	ordered := slices.Clone(ids)
	slices.Sort(ordered)
	out := make(map[string]*contracts.Shape, len(ids))
	for _, id := range slices.Compact(ordered) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		shape, err := point(s, contractShapeKey(id), decodeContractShape)
		if err != nil {
			s.retainContractRead(err)
			return nil, err
		}
		if shape != nil {
			out[id] = shape
		}
	}
	if err := errors.Join(ctx.Err(), s.ReadError()); err != nil {
		return nil, err
	}
	return out, nil
}

// FullNodesByIDsContext reconstructs complete Meta only for explicit full
// node reads. Ordinary node reads continue to return the saved N row alone.
func (s *Snapshot) FullNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
	if _, err := s.contractReadWitness(ctx); err != nil {
		return nil, err
	}
	if len(ids) > graph.ContractProjectionRowLimit {
		return nil, graph.ErrContractProjectionLimit
	}
	nodes := make(map[string]*graph.Node, len(ids))
	for id := range uniqueNonempty(ids) {
		n, err := s.projectedContractNode(ctx, id)
		if err != nil {
			s.retainContractRead(err)
			return nil, err
		}
		if n != nil {
			nodes[id] = n
		}
	}
	if err := errors.Join(ctx.Err(), s.ReadError()); err != nil {
		return nil, err
	}
	return nodes, nil
}

func (s *Snapshot) projectedContractNode(ctx context.Context, id string) (*graph.Node, error) {
	n, err := s.GetNodeContext(ctx, id)
	if err != nil || n == nil {
		return n, err
	}
	if n.ID != id {
		return nil, invalid("contract projection node differs from its key")
	}
	if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
		return n, nil
	}
	if _, exists := n.Meta["shape"]; exists {
		return nil, invalid("shape must come from the split contract family")
	}
	shape, err := point(s, contractShapeKey(id), decodeContractShape)
	if err != nil {
		return nil, err
	}
	if shape != nil {
		if n.Meta == nil {
			n.Meta = make(map[string]any, 1)
		}
		n.Meta["shape"] = shape
	}
	return n, nil
}
