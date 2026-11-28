package store_pebble

import (
	"context"
	"errors"
	"slices"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

var _ contracts.SnapshotReader = (*Snapshot)(nil)

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
	nodes, err := s.GetNodesByIDsContext(ctx, ids)
	if err != nil {
		s.retainContractRead(err)
		return nil, err
	}
	var types []string
	for id, n := range nodes {
		if n.ID != id {
			err := invalid("full node ID differs from storage key")
			s.retain(err)
			return nil, err
		}
		if n.Kind == graph.KindType || n.Kind == graph.KindInterface {
			if _, exists := n.Meta["shape"]; exists {
				err := invalid("shape must come from the split contract family")
				s.retain(err)
				return nil, err
			}
			types = append(types, id)
		}
	}
	shapes, err := s.contractShapes(ctx, types)
	if err != nil {
		return nil, err
	}
	for id, shape := range shapes {
		n := nodes[id] // the checked N decoder owns this fresh node and metadata
		if n.Meta == nil {
			n.Meta = make(map[string]any, 1)
		}
		n.Meta["shape"] = shape
	}
	return nodes, nil
}
