package storage

// NodeIdentityMaskKind separates node identity from outgoing-set ownership.
// Legacy is deliberately stronger: it retains ALL old tombstone behavior,
// including a same-generation row carried underneath the tombstone.
type NodeIdentityMaskKind string

type NodeIdentityMask struct {
	NodeID string
	Kind   NodeIdentityMaskKind
}

const (
	NodeIdentityMaskLegacy  NodeIdentityMaskKind = "legacy_tombstone"
	NodeIdentityMaskReplace NodeIdentityMaskKind = "identity_replace"
)
