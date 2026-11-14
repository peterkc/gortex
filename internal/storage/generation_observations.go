package storage

import "time"

// ContractCoreWindowGap is the quiet gap before one core-write window of a
// path opened: Quiet runs from the close of the path's previous window to
// this window's open, and OpenPrev is how long that previous window was
// open. Seq is the write seq that opened this window.
type ContractCoreWindowGap struct {
	Path            string
	Checkout        string
	ClosedAt        time.Time
	Seq             uint64
	OpenedAt        time.Time
	Quiet, OpenPrev time.Duration
}

// GenerationReferenceRelease is one committed catalog write that may have
// released generation references.
type GenerationReferenceRelease struct {
	// Removed is the generation whose catalog row this write deleted (0 for
	// none).
	Removed int64
	// Released names the generations whose references may have been
	// released. Empty with Any false means none.
	Released []int64
	// Any reports a release the write cannot name: any generation's reference
	// may have gone.
	Any bool
}

// GenerationWriteCounters is the physical write work one generation bulk
// window executed, measured at the writer rather than inferred from what a
// reader returns afterwards.
//
// The two measures answer different questions and are kept apart on purpose:
//
//   - StatementRowChanges is SQLite's own total_changes() delta on the pinned
//     writer connection across the window: every row an INSERT, UPDATE or
//     DELETE statement actually changed there — nodes, edges, sidecars, FTS
//     rowid maps, the FTS virtual tables, masks, producer states and the
//     withdrawal deletes of a fallback context separation alike. It is the
//     "rows the statements executed" count, not a count of rows that survived.
//   - BulkNodeRows and BulkEdgeRows are the node and edge rows the batch
//     writer committed through AddBatch inside the window (its own
//     changed/inserted accounting), the two payload tables the other measure
//     cannot split out.
//
// Neither counts reads, and neither counts rows written outside the window
// (a store without a pinned writer, such as an in-memory one, opens none).
type GenerationWriteCounters struct {
	GenerationID int64
	// WindowOpened reports that a generation bulk window was actually held; a
	// false value means the other fields are zero because nothing was
	// measured, not because nothing was written.
	WindowOpened        bool
	StatementRowChanges int64
	BulkNodeRows        int64
	BulkEdgeRows        int64
	// Batches counts the AddBatch commits the window observed.
	Batches int64
}

// PayloadGenerationRequest describes the generation to begin: who owns it,
// which layer it is built over, and the fingerprint of the inputs that
// produced it. The fingerprint fields are what make two begins for the same
// layer the same build.
type PayloadGenerationRequest struct {
	OwnerKind      string
	GraphID        string
	LayerID        string
	CheckoutID     string
	GenerationKind string

	// BaseGenerationID is the layer beneath this one, 0 for the base corpus.
	BaseGenerationID int64

	LowerViewFingerprint string
	TreeOID              string
	ProvenanceCommitOID  string
	ConfigHash           string
	ExtractorVersions    string
	ResolverVersion      string
	DependencyRevision   string

	CreatedAt int64 // unix seconds
}

// GenerationCopyCounts is what one row-level generation copy moved.
//
// Nodes and Edges are the two payload tables a caller reports on; Rows is the
// total across every generation-keyed table the copy touched, including the
// sidecars and the FTS projections. The authoritative description of what
// landed is the destination generation's own payload, which a caller reads
// back rather than trusting these numbers — they exist for the build report
// and the log line.
//
// The indexer's GenerationPayloadCopier interface aliases this type, so this
// declaration is what makes *Store satisfy it.
type GenerationCopyCounts struct {
	Nodes int64
	Edges int64
	Rows  int64
	// WALBytes is the log the copy's committed transaction appended (0 for
	// a rolled-back one: SQLite undoes its frames).
	WALBytes int64
}

// PayloadRetirementProgress reports committed work, including empty-source
// proofs that advanced the sealed generation's resume cursor. A refusal never
// reports deletion or catalog removal. Partial progress remains valid with err.
type PayloadRetirementProgress struct {
	RowsDeleted     int64
	ChunksCommitted int64
	StepsAdvanced   int
	CatalogRemoved  bool
}

// RowCounterDrift is one generation whose counter disagrees with its rows.
type RowCounterDrift struct {
	GenerationID             int64
	CounterNodes, ExactNodes int64
	CounterEdges, ExactEdges int64
}

// RowCounterCheck is the outcome of CheckRowCounters.
type RowCounterCheck struct {
	Ready       bool
	Generations int
	Drift       []RowCounterDrift
	Repaired    bool
	Elapsed     time.Duration
}
