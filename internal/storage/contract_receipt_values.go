package storage

import (
	"time"
)

// ContractReceiptWriteState is one path's write seq, whether the mutation
// that wrote it settled it, and whether a core-write window of the path is
// open.
type ContractReceiptWriteState struct {
	Seq     uint64
	Settled bool
	Writing bool
}

// ContractCoreWindow is one open core-write window of a primary path: the
// write seq that opened it and when. MarkedAt is when a failed mutation
// marked it orphaned, zero for a window not marked.
type ContractCoreWindow struct {
	Checkout, Path string
	OpenerSeq      uint64
	OpenedAt       time.Time
	MarkedAt       time.Time
}

// ContractBoundaryReceiptStamp is a receipt row's identity without its
// payload: what a contract job compares before and after a read.
type ContractBoundaryReceiptStamp struct {
	FilePath, Fingerprint, SourceFingerprint string
	Accepted                                 bool
}

// ContractCoreWindowClose is one bounded-history close used to associate a
// proof interval with a writer's next open, or identify a censored gap.
type ContractCoreWindowClose struct {
	Repo       string `json:"repo"`
	Checkout   string `json:"checkout"`
	Path       string `json:"path"`
	OpenerSeq  uint64 `json:"opener_seq"`
	ClosedAtNS int64  `json:"closed_at_ns"`
}
