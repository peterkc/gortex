package graph

import "iter"

// FrameworkCensusEdge is the typed projection consumed by framework admission
// and candidate multiplexing. It intentionally carries only the logical edge
// identity and the fixed metadata fields those read-only predicates inspect.
type FrameworkCensusEdge struct {
	EdgeIdentity
	Via                  string
	RegistryValue        string
	GRPCRegisterService  string
	GRPCService          string
	GRPCMethod           string
	HasExpressHandlerRef bool
	RecvConst            string
	ReceiverExpr         string
	RustPath             string
	RustRecv             string
	ReceiverType         string
	RustRecvExpr         string
	SynthesizedBy        string
}

// FrameworkCensusEdgeSequencer streams typed census projections. A consumer
// must exact-refetch the current full edge before mutation or before reading
// any metadata outside FrameworkCensusEdge.
type FrameworkCensusEdgeSequencer interface {
	FrameworkCensusEdgesSeq(kinds ...EdgeKind) iter.Seq[FrameworkCensusEdge]
}

// FrameworkCensusEdgesSeq selects the bounded projection when a backend
// provides it. The required Store iterator remains a correctness fallback for
// adapters; the production SQLite backend implements the projection directly.
func FrameworkCensusEdgesSeq(s Store, kinds ...EdgeKind) iter.Seq[FrameworkCensusEdge] {
	if s == nil || len(kinds) == 0 {
		return func(func(FrameworkCensusEdge) bool) {}
	}
	if seq, ok := s.(FrameworkCensusEdgeSequencer); ok {
		return seq.FrameworkCensusEdgesSeq(kinds...)
	}
	want := dedupeAnalysisEdgeKinds(kinds)
	return func(yield func(FrameworkCensusEdge) bool) {
		for _, kind := range want {
			for edge := range s.EdgesByKind(kind) {
				if edge == nil {
					continue
				}
				row := FrameworkCensusEdge{EdgeIdentity: EdgeIdentityFor(edge)}
				if edge.Meta != nil {
					row.Via, _ = edge.Meta["via"].(string)
					row.RegistryValue, _ = edge.Meta["registry_value"].(string)
					row.GRPCRegisterService, _ = edge.Meta["grpc_register_service"].(string)
					row.GRPCService, _ = edge.Meta["grpc_service"].(string)
					row.GRPCMethod, _ = edge.Meta["grpc_method"].(string)
					_, row.HasExpressHandlerRef = edge.Meta["express_handler_ref"]
					row.RecvConst, _ = edge.Meta["recv_const"].(string)
					row.ReceiverExpr, _ = edge.Meta["receiver_expr"].(string)
					row.RustPath, _ = edge.Meta["rust_path"].(string)
					row.RustRecv, _ = edge.Meta["rust_recv"].(string)
					row.ReceiverType, _ = edge.Meta["receiver_type"].(string)
					row.RustRecvExpr, _ = edge.Meta["rust_recv_expr"].(string)
					if edge.Kind == EdgeReferences || edge.Kind == EdgeImports {
						row.SynthesizedBy, _ = edge.Meta["synthesized_by"].(string)
					}
				}
				if !yield(row) {
					return
				}
			}
		}
	}
}
