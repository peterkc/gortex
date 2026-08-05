package graph

import "testing"

// TestClassifyNodePrefix pins the ownership rule the SQLite predicates in
// store_sqlite/prefix_diagnostics.go transcribe. The two must agree; a
// change here without a matching change there splits the backends.
func TestClassifyNodePrefix(t *testing.T) {
	cases := []struct {
		name string
		node *Node
		want string
	}{{
		name: "prefixed code node is owned and consistent",
		node: &Node{ID: "gortex/internal/foo.go::Bar", FilePath: "gortex/internal/foo.go", RepoPrefix: "gortex"},
		want: "",
	}, {
		name: "prefixed file node is owned and consistent",
		node: &Node{ID: "gortex/internal/foo.go", FilePath: "gortex/internal/foo.go", RepoPrefix: "gortex"},
		want: "",
	}, {
		name: "single-repo-mode code node is unowned",
		node: &Node{ID: "internal/foo.go::Bar", FilePath: "internal/foo.go"},
		want: PrefixIssueUnowned,
	}, {
		// The population the whole refactor exists to eliminate: a repo
		// prefix stamped on the field while the identity was minted bare.
		name: "stamped prefix that the identity does not carry",
		node: &Node{ID: "internal/foo.go::Bar", FilePath: "internal/foo.go", RepoPrefix: "gortex"},
		want: PrefixIssueMisprefixed,
	}, {
		name: "file path prefixed but id is not",
		node: &Node{ID: "internal/foo.go::Bar", FilePath: "gortex/internal/foo.go", RepoPrefix: "gortex"},
		want: PrefixIssueMisprefixed,
	}, {
		// A repo whose name is a prefix of another must not satisfy the
		// check by accident — the separator is part of the match.
		name: "sibling repo whose name is a string prefix",
		node: &Node{ID: "gortex-bench/x.go::A", FilePath: "gortex-bench/x.go", RepoPrefix: "gortex"},
		want: PrefixIssueMisprefixed,
	}, {
		name: "global external stub carries no file and is not audited",
		node: &Node{ID: "dep::go.uber.org/zap::Logger", Kind: KindFunction},
		want: "",
	}, {
		name: "semantic external virtual path is not audited",
		node: &Node{ID: "external::go:crypto/sha256::Size", Kind: KindConstant, FilePath: "external::go:crypto/sha256"},
		want: "",
	}, {
		name: "external-call module virtual path is not audited",
		node: &Node{ID: "external-call::npm:lodash", Kind: KindModule, FilePath: "external-call::npm:lodash"},
		want: "",
	}, {
		name: "contract identity namespace is not audited",
		node: &Node{ID: "contract::http::GET::/health", Kind: KindContract, FilePath: "gortex/api.go", RepoPrefix: "gortex"},
		want: "",
	}, {
		name: "contract bridge virtual identity is not audited",
		node: &Node{ID: "contract-bridge::request::response", Kind: KindContractBridge, FilePath: "contracts://bridges", RepoPrefix: "gortex"},
		want: "",
	}, {
		name: "topic identity namespace is not audited",
		node: &Node{ID: "topic::nats::go", Kind: KindTopic, FilePath: "gortex/main.go", RepoPrefix: "gortex"},
		want: "",
	}, {
		name: "repo-scoped stub carries no file and is not audited",
		node: &Node{ID: "gortex::builtin::go::make", Kind: KindBuiltin, RepoPrefix: "gortex"},
		want: "",
	}, {
		name: "unresolved sentinel is not audited",
		node: &Node{ID: "unresolved::Name"},
		want: "",
	}, {
		name: "nil node",
		node: nil,
		want: "",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyNodePrefix(tc.node); got != tc.want {
				t.Fatalf("ClassifyNodePrefix() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPrefixDiagnosticsMixed is the ghost-graph detector. Neither indexing
// mode produces a store holding both populations, so Mixed() firing always
// means one of them is stale residue.
func TestPrefixDiagnosticsMixed(t *testing.T) {
	owned := &Node{ID: "gortex/a.go::A", FilePath: "gortex/a.go", RepoPrefix: "gortex"}
	unowned := &Node{ID: "a.go::A", FilePath: "a.go"}
	global := &Node{ID: "dep::zap::Logger"}

	t.Run("wholly prefixed is clean and not mixed", func(t *testing.T) {
		d := AuditNodePrefixes([]*Node{owned, global}, 3)
		if d.Mixed() || !d.Clean() {
			t.Fatalf("mixed=%v clean=%v, want false/true (%s)", d.Mixed(), d.Clean(), d.Summary())
		}
		if d.OwnedCodeNodes != 1 || d.UnownedCodeNodes != 0 {
			t.Fatalf("owned=%d unowned=%d, want 1/0", d.OwnedCodeNodes, d.UnownedCodeNodes)
		}
	})

	t.Run("wholly unprefixed is single-repo mode, not a defect", func(t *testing.T) {
		d := AuditNodePrefixes([]*Node{unowned, global}, 3)
		if d.Mixed() {
			t.Fatalf("Mixed() = true for a wholly-unprefixed graph (%s)", d.Summary())
		}
		if !d.Clean() {
			t.Fatalf("Clean() = false for single-repo mode (%s)", d.Summary())
		}
		if d.UnownedCodeNodes != 1 {
			t.Fatalf("unowned = %d, want 1", d.UnownedCodeNodes)
		}
	})

	t.Run("both populations present is the ghost graph", func(t *testing.T) {
		d := AuditNodePrefixes([]*Node{owned, unowned, global}, 3)
		if !d.Mixed() {
			t.Fatalf("Mixed() = false with both populations present (%s)", d.Summary())
		}
		if d.Clean() {
			t.Fatal("Clean() = true for a mixed graph")
		}
		if len(d.UnownedSamples) != 1 || d.UnownedSamples[0] != unowned.ID {
			t.Fatalf("UnownedSamples = %v, want [%s]", d.UnownedSamples, unowned.ID)
		}
	})

	t.Run("sample limit is respected", func(t *testing.T) {
		nodes := make([]*Node, 0, 10)
		for i := range 10 {
			nodes = append(nodes, &Node{ID: string(rune('a'+i)) + ".go::X", FilePath: string(rune('a'+i)) + ".go"})
		}
		d := AuditNodePrefixes(nodes, 3)
		if d.UnownedCodeNodes != 10 {
			t.Fatalf("unowned = %d, want 10", d.UnownedCodeNodes)
		}
		if len(d.UnownedSamples) != 3 {
			t.Fatalf("samples = %d, want 3 (the limit)", len(d.UnownedSamples))
		}
	})
}

// TestGraphPrefixDiagnostics exercises the in-memory shard walk against the
// same expectations as the slice-based fallback.
func TestGraphPrefixDiagnostics(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "gortex/a.go::A", Kind: KindFunction, Name: "A", FilePath: "gortex/a.go", RepoPrefix: "gortex"})
	g.AddNode(&Node{ID: "b.go::B", Kind: KindFunction, Name: "B", FilePath: "b.go"})
	g.AddNode(&Node{ID: "dep::zap::Logger", Kind: KindFunction, Name: "Logger"})

	d := g.PrefixDiagnostics(5)
	if d.OwnedCodeNodes != 1 {
		t.Fatalf("owned = %d, want 1 (%s)", d.OwnedCodeNodes, d.Summary())
	}
	if d.UnownedCodeNodes != 1 {
		t.Fatalf("unowned = %d, want 1 (%s)", d.UnownedCodeNodes, d.Summary())
	}
	if !d.Mixed() {
		t.Fatalf("Mixed() = false, want true (%s)", d.Summary())
	}
	if d.Scanned != 3 {
		t.Fatalf("scanned = %d, want 3", d.Scanned)
	}
}

// TestReadPrefixDiagnosticsPrefersCapability proves the probe takes the
// native path when the store offers one, so a large SQLite store is never
// audited by materialising every node.
func TestReadPrefixDiagnosticsPrefersCapability(t *testing.T) {
	sentinel := PrefixDiagnostics{OwnedCodeNodes: 7, Scanned: 7}
	got, ok := ReadPrefixDiagnostics(stubPrefixReader{d: sentinel}, 5)
	if !ok {
		t.Fatal("ReadPrefixDiagnostics reported no capability")
	}
	if got.OwnedCodeNodes != 7 {
		t.Fatalf("owned = %d, want 7 — the AllNodes fallback ran instead", got.OwnedCodeNodes)
	}

	if _, ok := ReadPrefixDiagnostics(struct{}{}, 5); ok {
		t.Fatal("a store with neither capability reported ok=true, which would read as a false clean")
	}
}

type stubPrefixReader struct{ d PrefixDiagnostics }

func (s stubPrefixReader) PrefixDiagnostics(int) PrefixDiagnostics { return s.d }

// AllNodes would be preferred only if the capability probe missed.
func (s stubPrefixReader) AllNodes() []*Node {
	return []*Node{{ID: "x.go::X", FilePath: "x.go"}}
}
