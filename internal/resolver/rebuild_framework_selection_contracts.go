package resolver

// WithRebuildEarlyContractIDs gives only the private framework readers the
// exact pre-framework contract IDs and their native file placements. It does
// not add nodes or edges to the working graph.
func (s FrameworkSynthesizerSelection) WithRebuildEarlyContractIDs(ids map[string]string) FrameworkSynthesizerSelection {
	s.rebuildEarlyContractIDs = ids
	return s
}
