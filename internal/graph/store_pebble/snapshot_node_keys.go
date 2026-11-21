package store_pebble

import (
	"context"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.NodeKindsByIDsReader     = (*Snapshot)(nil)
	_ graph.NodeKindMembershipReader = (*Snapshot)(nil)
	_ graph.NodePresenceByIDsReader  = (*Snapshot)(nil)
	_ graph.NodeSearchKeyScanner     = (*Snapshot)(nil)
	_ graph.PrefixDiagnosticsReader  = (*Snapshot)(nil)
)

func (s *Snapshot) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]graph.NodeKindRow, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	out := make(map[string]graph.NodeKindRow)
	for id := range uniqueNonempty(ids) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := point(s, keyStrings([]byte{familyNode}, id), decodeNodeLight)
		if err != nil {
			return nil, err
		}
		if n != nil {
			out[id] = graph.NodeKindRow{Kind: n.Kind, FilePath: n.FilePath, RepoPrefix: n.RepoPrefix}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Snapshot) GetNodeIDsByKindsContext(ctx context.Context, ids []string, kinds []graph.NodeKind) (map[string]struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]struct{})
	if len(kinds) == 0 {
		return out, nil
	}
	for id := range uniqueNonempty(ids) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := point(s, keyStrings([]byte{familyNode}, id), decodeNodeLight)
		if err != nil {
			return nil, err
		}
		if n != nil && slices.Contains(kinds, n.Kind) {
			out[id] = struct{}{}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Snapshot) GetNodePresenceByIDsContext(ctx context.Context, ids []string) (map[string]struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	out := make(map[string]struct{})
	for id := range uniqueNonempty(ids) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		found, err := point(s, keyStrings([]byte{familyNode}, id), func([]byte) (bool, error) { return true, nil })
		if err != nil {
			return nil, err
		}
		if found {
			out[id] = struct{}{}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Snapshot) ScanNodeSearchKeys(ctx context.Context, pageSize int, yield func([]graph.NodeSearchKey) bool) error {
	if yield == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if pageSize <= 0 {
		pageSize = graph.DefaultNodeSearchKeyPageSize
	}
	lower, upper := []byte{familyNode}, prefixEnd([]byte{familyNode})
	for {
		var page []graph.NodeSearchKey
		err := scanRange(ctx, s, lower, upper, func(k, raw []byte) (bool, error) {
			n, err := decodeNodeLight(raw)
			if err != nil {
				return false, err
			}
			page = append(page, graph.NodeSearchKey{ID: n.ID, Kind: n.Kind, Name: n.Name,
				FilePath: n.FilePath, RepoPrefix: n.RepoPrefix, WorkspaceID: n.WorkspaceID, ProjectID: n.ProjectID})
			if len(page) == pageSize {
				// A complete node key has no key suffix. Resume after it on a new cursor.
				lower = prefixEnd(k)
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		if !yield(page) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

func (s *Snapshot) PrefixDiagnostics(sampleLimit int) graph.PrefixDiagnostics {
	var out graph.PrefixDiagnostics
	err := scan(context.Background(), s, []byte{familyNode}, func(_, raw []byte) (bool, error) {
		n, err := decodeNodeLight(raw)
		if err != nil {
			return false, err
		}
		out.Scanned++
		switch graph.ClassifyNodePrefix(n) {
		case graph.PrefixIssueUnowned:
			out.UnownedCodeNodes++
			if len(out.UnownedSamples) < sampleLimit {
				out.UnownedSamples = append(out.UnownedSamples, n.ID)
			}
		case graph.PrefixIssueMisprefixed:
			out.MisprefixedNodes++
			if len(out.MisprefixedSamples) < sampleLimit {
				out.MisprefixedSamples = append(out.MisprefixedSamples, n.ID)
			}
		default:
			if graph.IsAuditableRepoSourceNode(n) {
				out.OwnedCodeNodes++
			}
		}
		return true, nil
	})
	if err != nil {
		s.retain(err)
		return graph.PrefixDiagnostics{}
	}
	return out
}
