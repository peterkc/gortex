package storage

import "fmt"

// CheckoutPathEvidence is the last filesystem sample taken for a checkout's
// path: enough to tell a deleted working copy from a detached volume.
type CheckoutPathEvidence struct {
	CheckoutID                  string
	RootPathIdentity            string
	RootVolumeKind              string
	RootVolumeToken             string
	NearestExistingAncestorPath string
	AncestorVolumeKind          string
	AncestorVolumeToken         string
	CommonDirVolumeKind         string
	CommonDirVolumeToken        string
	SampledAt                   int64 // unix seconds
	SampleGeneration            int64
}

// ViewGenerationReferences is which kinds of pointer still name a generation.
// It is the reference guard's verdict broken out by holder, so a refused
// retirement can say why it was refused instead of only that it was.
type ViewGenerationReferences struct {
	// Routed is a checkout route naming the generation in either slot.
	Routed bool
	// RefViewed is a ref view serving it as its active generation.
	RefViewed bool
	// Based is another generation naming it as the layer beneath.
	Based bool
	// GraphActive is a dedicated graph's active pointer.
	GraphActive bool
	// DedicatedPublication is the graph's current build/publication association.
	DedicatedPublication bool
	ContractAttached     bool
}

// GenerationReferencedError is a retirement refused for the references the
// generation still had, when the refusal knows them. It matches
// ErrCatalogGenerationReferenced.
type GenerationReferencedError struct {
	GenerationID int64
	Refs         ViewGenerationReferences
}

// Any reports whether any pointer names the generation. It is the boolean the
// delete guard enforces.
func (r ViewGenerationReferences) Any() bool {
	return r.Routed || r.RefViewed || r.Based || r.GraphActive || r.DedicatedPublication || r.ContractAttached
}

func (e *GenerationReferencedError) Error() string {
	return fmt.Sprintf("%v: generation %d", ErrCatalogGenerationReferenced, e.GenerationID)
}

func (e *GenerationReferencedError) Unwrap() error { return ErrCatalogGenerationReferenced }

// OnlyBased reports references that are nothing but generations built on
// this one: the one kind a release can always name, since only removing or
// re-basing the generation above releases it.
func (r ViewGenerationReferences) OnlyBased() bool {
	return r.Based && !r.Routed && !r.RefViewed && !r.GraphActive && !r.DedicatedPublication && !r.ContractAttached
}
