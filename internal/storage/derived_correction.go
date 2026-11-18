package storage

import (
	"errors"
	"github.com/zzet/gortex/internal/graph"
	"time"
)

// StaleDerivationGeneration is one live generation whose stamp for a pass is
// older than the running version.
type StaleDerivationGeneration struct {
	GenerationID int64
	State        ViewGenerationState
	Version      int // 0: no stamp
}

// DerivedCorrectionRequest names what one correction may write.
type DerivedCorrectionRequest struct {
	GenerationID int64
	Pass         string
	// FromVersion is the stamp the correction expects (0: no stamp);
	// ToVersion the one it records when it finishes (> FromVersion).
	FromVersion, ToVersion int
	// EdgeKinds are the pass's derived edge kinds: the only edges a chunk may
	// delete or insert.
	EdgeKinds []graph.EdgeKind
}

// DerivedCorrectionStats is what a correction wrote so far.
type DerivedCorrectionStats struct {
	Chunks int
	// ChangedChunks are the chunks that changed a row; the others found the
	// pass's rows already as given and wrote nothing.
	ChangedChunks int
	Sources       int
	EdgesDeleted  int64
	EdgesInserted int64
	NodesUpdated  int64
	MaxChunkHold  time.Duration
	Elapsed       time.Duration
}

var (
	// ErrDerivationStampInvalid: an empty pass name or a version below 1.
	ErrDerivationStampInvalid = errors.New("store_sqlite: invalid derivation stamp")
	// ErrDerivationStampMoved: the generation's stamp for the pass is no longer
	// the version the correction started from (another correction finished).
	ErrDerivationStampMoved = errors.New("store_sqlite: derivation stamp moved during the correction")
	// ErrDerivedCorrectionRefused: the generation cannot be corrected (it is
	// retiring, failed, or has no catalog row), or a chunk carried a row the
	// correction may not write.
	ErrDerivedCorrectionRefused = errors.New("store_sqlite: derived-row correction refused")
)

// DerivedCorrectionMaxChunkRows bounds the rows one chunk may write (edges
// plus node rows). At the ≈0.1 ms per row a cold one-file delta costs on the
// live-sized store (write_rows probe), 4,096 rows hold the writer well under
// the reclaim's 2 s cap. Callers split larger corrections.
const DerivedCorrectionMaxChunkRows = 4096
