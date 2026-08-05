package graph

import (
	"strconv"
	"strings"
)

// Repo-prefix diagnostics.
//
// A node's repo prefix is the graph's ownership key: it keys the byRepo
// bucket, the per-repo memory estimates, the SQLite `nodes_by_repo` partial
// index, and every repo-scoped query. When ownership is wrong, nothing
// errors — the graph simply answers with a subset, or with two copies of
// the same repo under different IDs. Both failures read as "green" to
// index_health, so they need an explicit detector rather than an assertion
// that would never fire.
//
// The audit applies only to repository source nodes. Some synthetic nodes
// retain a FilePath as attribution rather than as an on-disk source path:
// semantic externals use `external::…`, synthesized external calls use
// `external-call::…`, and contracts plus topics use identity namespaces that
// deliberately do not mirror their source path. Those populations are outside
// the ownership invariant even when FilePath is non-empty.
//
// Every other file-backed node with an empty prefix is an UNOWNED CODE NODE:
// a real symbol, extracted from a real file, that no repository claims.

// PrefixDiagnostics is a bounded ownership audit of the node set.
type PrefixDiagnostics struct {
	// OwnedCodeNodes / UnownedCodeNodes split the auditable repository-source
	// node set by whether a repository claims it.
	//
	// While single-repo mode exists, a lone-repo daemon legitimately reports
	// the whole graph as unowned — that IS single-repo mode. What is never
	// legitimate is BOTH being non-zero: that means a stale unprefixed
	// population survived alongside a freshly prefixed one and every count,
	// scope filter and per-repo bucket now sees half the graph. See Mixed.
	OwnedCodeNodes   int
	UnownedCodeNodes int

	// MisprefixedNodes counts owned nodes (RepoPrefix != "") whose ID or
	// FilePath does not actually start with "<RepoPrefix>/". The prefix is
	// applied by string concatenation in Indexer.applyRepoPrefix, so a
	// divergence here means the stamped field and the minted identity
	// disagree and repo-scoped lookups will miss the node.
	MisprefixedNodes int

	// UnownedSamples / MisprefixedSamples hold up to sampleLimit node IDs
	// from each population, so a log line can name the offenders instead of
	// only counting them.
	UnownedSamples     []string
	MisprefixedSamples []string

	// Scanned is the number of nodes actually examined. A backend may cap
	// the walk; Truncated records that the counts are lower bounds.
	Scanned   int
	Truncated bool
}

// Mixed reports the ghost-graph signature: the same store holding both
// owned and unowned code nodes. No indexing mode produces this — a graph is
// either wholly prefixed or wholly unprefixed — so it always means one
// population is stale residue that every repo-scoped read is silently
// splitting on.
func (d PrefixDiagnostics) Mixed() bool {
	return d.OwnedCodeNodes > 0 && d.UnownedCodeNodes > 0
}

// Clean reports whether the audit found no ownership problems. It tolerates
// a wholly-unowned graph, which is what single-repo mode looks like.
func (d PrefixDiagnostics) Clean() bool {
	return !d.Mixed() && d.MisprefixedNodes == 0
}

// Summary renders the audit as a short human-readable clause for a log line
// or a recommendation string.
func (d PrefixDiagnostics) Summary() string {
	var b strings.Builder
	b.WriteString("owned=")
	b.WriteString(strconv.Itoa(d.OwnedCodeNodes))
	b.WriteString(" unowned=")
	b.WriteString(strconv.Itoa(d.UnownedCodeNodes))
	if d.MisprefixedNodes > 0 {
		b.WriteString(" misprefixed=")
		b.WriteString(strconv.Itoa(d.MisprefixedNodes))
	}
	if len(d.UnownedSamples) > 0 {
		b.WriteString(" e.g. ")
		b.WriteString(strings.Join(d.UnownedSamples, ", "))
	}
	return b.String()
}

// PrefixDiagnosticsReader is the optional store capability that answers the
// audit natively. The SQLite store implements it with two aggregate queries;
// callers that probe and miss fall back to AuditNodePrefixes over a node
// slice.
type PrefixDiagnosticsReader interface {
	PrefixDiagnostics(sampleLimit int) PrefixDiagnostics
}

// NodePrefixOwnership classifies one node for the audit. It is the single
// definition both the in-memory walk and the SQLite predicates encode, so
// the two backends cannot drift.
//
// Returns ("", true) for a node that is fine or out of scope (synthetic
// global externals), and a reason string for a node that is not.
const (
	// PrefixIssueUnowned marks a code node no repository claims.
	PrefixIssueUnowned = "unowned_code_node"
	// PrefixIssueMisprefixed marks an owned node whose identity does not
	// carry the prefix its RepoPrefix field claims.
	PrefixIssueMisprefixed = "misprefixed_identity"
)

const (
	externalSemanticFilePathPrefix = "external::"
	externalCallFilePathPrefix     = "external-call::"
)

// IsAuditableRepoSourcePath reports whether filePath names repository source
// rather than a synthetic attribution bucket. The exact same path prefixes
// are excluded by the SQLite ownership predicates.
func IsAuditableRepoSourcePath(filePath string) bool {
	return filePath != "" &&
		!strings.HasPrefix(filePath, externalSemanticFilePathPrefix) &&
		!strings.HasPrefix(filePath, externalCallFilePathPrefix)
}

// IsAuditableRepoSourceNode reports whether n participates in the repository
// ownership invariant. Contract and topic identities intentionally live in
// global namespaces rather than below their source repository prefix; contract
// bridge nodes likewise use the virtual contracts://bridges path.
func IsAuditableRepoSourceNode(n *Node) bool {
	if n == nil || !IsAuditableRepoSourcePath(n.FilePath) {
		return false
	}
	switch n.Kind {
	case KindContract, KindContractBridge, KindTopic:
		return false
	default:
		return true
	}
}

// ClassifyNodePrefix returns "" when n's ownership is consistent or outside
// the repository-source audit, and one of the PrefixIssue* constants otherwise.
// For audited nodes, both ID and FilePath are produced by
// Indexer.applyRepoPrefix concatenating `repoPrefix + "/"`, so both must carry
// it.
func ClassifyNodePrefix(n *Node) string {
	if !IsAuditableRepoSourceNode(n) {
		return ""
	}
	if n.RepoPrefix == "" {
		return PrefixIssueUnowned
	}
	want := n.RepoPrefix + "/"
	if !strings.HasPrefix(n.FilePath, want) || !strings.HasPrefix(n.ID, want) {
		return PrefixIssueMisprefixed
	}
	return ""
}

// accumulate folds one node into the audit.
func (d *PrefixDiagnostics) accumulate(n *Node, sampleLimit int) {
	if n == nil {
		return
	}
	d.Scanned++
	switch ClassifyNodePrefix(n) {
	case PrefixIssueUnowned:
		d.UnownedCodeNodes++
		if len(d.UnownedSamples) < sampleLimit {
			d.UnownedSamples = append(d.UnownedSamples, n.ID)
		}
	case PrefixIssueMisprefixed:
		d.MisprefixedNodes++
		if len(d.MisprefixedSamples) < sampleLimit {
			d.MisprefixedSamples = append(d.MisprefixedSamples, n.ID)
		}
	default:
		// Healthy. Only audited repository-source nodes count toward
		// ownership; synthetic attribution paths and identity namespaces do
		// not belong to either population.
		if IsAuditableRepoSourceNode(n) {
			d.OwnedCodeNodes++
		}
	}
}

// AuditNodePrefixes runs the ownership audit over an explicit node slice.
// It is the backend-independent fallback for stores that do not implement
// PrefixDiagnosticsReader.
func AuditNodePrefixes(nodes []*Node, sampleLimit int) PrefixDiagnostics {
	var d PrefixDiagnostics
	for _, n := range nodes {
		d.accumulate(n, sampleLimit)
	}
	return d
}

// PrefixDiagnostics walks every shard and audits node ownership. The
// in-memory store is the reference implementation; it holds each shard's
// read lock only for that shard's pass.
func (g *Graph) PrefixDiagnostics(sampleLimit int) PrefixDiagnostics {
	var d PrefixDiagnostics
	for _, s := range g.shards {
		s.mu.RLock()
		for _, n := range s.nodes {
			d.accumulate(n, sampleLimit)
		}
		s.mu.RUnlock()
	}
	return d
}

// ReadPrefixDiagnostics runs the audit against any store, preferring the
// native capability and falling back to a full node pull only when the
// backend cannot answer it directly. Returns ok=false when neither path is
// available, so a caller can stay silent rather than report a false clean.
func ReadPrefixDiagnostics(store any, sampleLimit int) (PrefixDiagnostics, bool) {
	if store == nil {
		return PrefixDiagnostics{}, false
	}
	if r, capable := store.(PrefixDiagnosticsReader); capable {
		return r.PrefixDiagnostics(sampleLimit), true
	}
	if all, capable := store.(interface{ AllNodes() []*Node }); capable {
		return AuditNodePrefixes(all.AllNodes(), sampleLimit), true
	}
	return PrefixDiagnostics{}, false
}
