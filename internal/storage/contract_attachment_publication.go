package storage

import (
	"github.com/zzet/gortex/internal/graph"
)

// ContractAttachmentPublishTarget binds a captured input to its selected core
// handle. Every handle in a batch must share the receiver's store core.
type ContractAttachmentPublishTarget struct {
	// ProjectionWitness is job-local proof of every selected physical input.
	// Incremental consumers must capture it before their checked reads; it is
	// never stored in an actor checkpoint or substituted for receipt/work CAS.
	ProjectionWitness *PayloadInputWitness
	Store             graph.Store
	Expected          graph.ContractInputState
	Witnesses         []graph.ContractInputWitness
	Attachment        graph.ContractAttachment
	Work              []graph.ContractWork
}

// ContractAttachmentPublication reports the immutable winning payload and the
// losing reservation, if any. The caller owns retirement of unused reservations.
type ContractAttachmentPublication struct {
	PayloadGeneration       int64
	UnusedPayloadGeneration int64
}
