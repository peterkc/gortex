package graph

// ReceiverMutationMethod is the compact method declaration needed by the
// whole-graph receiver-mutation fixpoint.
type ReceiverMutationMethod struct {
	ID         string
	Receiver   string
	RepoPrefix string
	FilePath   string
	Language   string
	ScopePkg   string
}

// ReceiverMutationField is the compact field declaration needed by the
// whole-graph receiver-mutation fixpoint.
type ReceiverMutationField struct {
	ID         string
	Name       string
	Receiver   string
	RepoPrefix string
	FilePath   string
	Language   string
	ScopePkg   string
}

// ReceiverMutationWrite is the compact direct-write seed for the
// receiver-mutation fixpoint.
type ReceiverMutationWrite struct {
	From string
	To   string
}

// ReceiverMutationCall is an own-receiver call candidate. TargetMethodName is
// populated only when To currently resolves to a valid method node; callers
// otherwise derive the legacy bare target name from To.
type ReceiverMutationCall struct {
	From             string
	To               string
	TargetMethodName string
	RecvField        string
	RecvSelf         bool
	FilePath         string
	Line             int
}

// ReceiverMutationScanner streams the four compact input relations used by
// the whole-graph receiver-mutation fixpoint. Implementations must freeze a
// high-water mark for each scan, return bounded pages, and close a backend
// cursor before invoking yield so callbacks may safely re-enter the Store.
// Rows whose persisted metadata cannot be decoded must be omitted, matching
// the full-node/full-edge Store iterators.
type ReceiverMutationScanner interface {
	ScanReceiverMutationMethods(pageSize int, yield func([]ReceiverMutationMethod) bool)
	ScanReceiverMutationFields(pageSize int, yield func([]ReceiverMutationField) bool)
	ScanReceiverMutationWrites(pageSize int, yield func([]ReceiverMutationWrite) bool)
	ScanReceiverMutationCalls(pageSize int, yield func([]ReceiverMutationCall) bool)
}
