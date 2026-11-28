package resolver

// WithRebuildEarlyContractIDs lets gRPC require a call receipt without
// requiring a parse node receipt for an overlaid contract caller.
func (s FrameworkSynthesizerSelection) WithRebuildEarlyContractIDs(ids map[string]string) FrameworkSynthesizerSelection {
	s.rebuildEarlyContractIDs = ids
	return s
}
