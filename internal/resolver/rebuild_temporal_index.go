package resolver

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	temporalIndexRegistered byte = 'I' // native kind::name, append ordinal -> node ID
	temporalIndexJavaMethod byte = 'L' // file or receiver, value, method ID
	temporalIndexJavaTag    byte = 'M' // interface ID, annotation ordinal
	temporalIndexJavaMember byte = 'P' // interface ID, method name
	temporalIndexGoSnapshot byte = 'T' // node ID, role or name -> register snapshot value
)

// A repeated ID is still a distinct row. Save only the first ID, and never
// choose it once more than one row qualifies at this locality.
type rebuildTemporalUnique struct {
	id        string
	ambiguous bool
}

func (u *rebuildTemporalUnique) add(id string) {
	if u.id == "" {
		u.id = id
		return
	}
	u.ambiguous = true
}

func (u rebuildTemporalUnique) winner() string {
	if u.ambiguous {
		return ""
	}
	return u.id
}

func (p *rebuildTemporal) buildIndex() error {
	var next uint64
	appendRow := func(kind, name, id string) error {
		next++
		return p.s.Put(p.ctx, temporalIndexRegistered, id, kind+"::"+name, temporalOrdinal(next))
	}
	if err := p.visitTemporalCalls(func(e *graph.Edge) error {
		if via, _ := e.Meta["via"].(string); via != "temporal.register" {
			return nil
		}
		kind, _ := e.Meta["temporal_kind"].(string)
		name, _ := e.Meta["temporal_name"].(string)
		if e.From == "" || kind == "" || name == "" {
			return nil
		}
		caller, err := p.s.Node(p.ctx, e.From)
		if err != nil || caller == nil {
			return err
		}
		plural, _ := e.Meta["temporal_register_plural"].(bool)
		target, err := p.pickTemporalGoTarget(name, caller, plural)
		if err != nil || target == nil {
			return err
		}
		if plural {
			return p.s.Incoming(p.ctx, target.ID, graph.EdgeMemberOf, func(link *graph.Edge) error {
				if link.From == "" {
					return nil
				}
				m, err := p.s.Node(p.ctx, link.From)
				if err != nil || m == nil {
					return err
				}
				if m.Language != "go" || m.Kind != graph.KindMethod || !isExportedGoName(m.Name) {
					return nil
				}
				if err := p.stampTemporalGo(e, m, kind, m.Name, true); err != nil {
					return err
				}
				return appendRow(kind, m.Name, m.ID)
			})
		}
		regName, _ := e.Meta["temporal_registered_name"].(string)
		if regName == "" {
			regName = name
		}
		if err := p.stampTemporalGo(e, target, kind, regName, false); err != nil {
			return err
		}
		return appendRow(kind, regName, target.ID)
	}); err != nil {
		return err
	}
	return p.indexTemporalJava(appendRow)
}

func (p *rebuildTemporal) pickTemporalGoTarget(name string, caller *graph.Node, plural bool) (*graph.Node, error) {
	var file, repo, all rebuildTemporalUnique
	view := p.s.View()
	err := view.VisitNameCandidates(p.ctx, name, []string{"go"}, func(n *graph.Node) error {
		if plural {
			if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
				return nil
			}
		} else if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			return nil
		}
		all.add(n.ID)
		if caller.RepoPrefix != "" && n.RepoPrefix == caller.RepoPrefix {
			repo.add(n.ID)
		}
		if n.FilePath == caller.FilePath {
			file.add(n.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, u := range [...]rebuildTemporalUnique{file, repo, all} {
		if id := u.winner(); id != "" {
			return p.s.Node(p.ctx, id)
		}
	}
	return nil, nil
}

func (p *rebuildTemporal) stampTemporalGo(e *graph.Edge, n *graph.Node, role, name string, plural bool) error {
	curRole, _ := n.Meta["temporal_role"].(string)
	curName, _ := n.Meta["temporal_name"].(string)

	// SQLite's nonplural FindNodesByNames batch retains the same decoded
	// object across registrations. Plural GetNodesByIDs reads fresh objects
	// and does not update that batch. Memoize the separate role/name only
	// once a plural write can make the two views diverge.
	savedRole, saved, err := p.s.Get(p.ctx, temporalIndexGoSnapshot, n.ID, "role")
	if err != nil {
		return err
	}
	if plural {
		if curRole == role && (name == "" || curName == name) {
			return nil
		}
		if !saved {
			if err := p.saveTemporalGoSnapshot(n.ID, curRole, curName); err != nil {
				return err
			}
		}
	} else {
		if saved {
			curRole = savedRole
			curName, _, err = p.s.Get(p.ctx, temporalIndexGoSnapshot, n.ID, "name")
			if err != nil {
				return err
			}
		}
		if curRole == role && (name == "" || curName == name) {
			return nil
		}
		if saved {
			if err := p.saveTemporalGoSnapshot(n.ID, role, name); err != nil {
				return err
			}
		}
	}
	return p.writeTemporalIndexRole(e, n, role, name)
}

func (p *rebuildTemporal) saveTemporalGoSnapshot(id, role, name string) error {
	if err := p.s.Put(p.ctx, temporalIndexGoSnapshot, role, id, "role"); err != nil {
		return err
	}
	return p.s.Put(p.ctx, temporalIndexGoSnapshot, name, id, "name")
}

func (p *rebuildTemporal) stampTemporalJava(e *graph.Edge, n *graph.Node, role, name string) error {
	if cur, _ := n.Meta["temporal_role"].(string); cur == role {
		if curName, _ := n.Meta["temporal_name"].(string); name == "" || curName == name {
			return nil
		}
	}
	return p.writeTemporalIndexRole(e, n, role, name)
}

func (p *rebuildTemporal) writeTemporalIndexRole(e *graph.Edge, n *graph.Node, role, name string) error {
	if err := p.s.WantRecorder(p.ctx, e); err != nil {
		return err
	}
	if err := p.s.WantNode(p.ctx, n.ID); err != nil {
		return err
	}
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	n.Meta["temporal_role"] = role
	if name != "" {
		n.Meta["temporal_name"] = name
	}
	return p.s.StampNode(p.ctx, n)
}

func (p *rebuildTemporal) indexTemporalJava(appendRow func(kind, name, id string) error) error {
	view := p.s.View()
	var tags uint64
	if err := p.visitTemporalAnnotations(func(e *graph.Edge) error {
		ifaceRole, methodRole := temporalRoleForJavaAnnotation(e.To)
		if e.From == "" || (ifaceRole == "" && methodRole == "") {
			return nil
		}
		from, err := p.s.Node(p.ctx, e.From)
		if err != nil || from == nil {
			return err
		}
		if methodRole != "" && (from.Kind == graph.KindMethod || from.Kind == graph.KindFunction) {
			args, _ := e.Meta["args"].(string)
			canonical := javaMethodCanonicalName(methodRole, from.Name, args)
			if err := p.stampTemporalJava(e, from, methodRole, canonical); err != nil {
				return err
			}
			return appendRow(normaliseTemporalKind(methodRole), canonical, from.ID)
		}
		if ifaceRole == "" || from.Kind != graph.KindInterface {
			return nil
		}
		if err := p.stampTemporalJava(e, from, ifaceRole, from.Name); err != nil {
			return err
		}
		tags++
		return p.s.Put(p.ctx, temporalIndexJavaTag, "", from.ID, temporalOrdinal(tags))
	}); err != nil || tags == 0 {
		return err
	}

	if err := view.VisitNodes(p.ctx, func(n *graph.Node) error {
		if n.Kind != graph.KindMethod || n.Language != "java" {
			return nil
		}
		if recv, _ := n.Meta["receiver"].(string); recv != "" {
			return p.s.Put(p.ctx, temporalIndexJavaMethod, "", "receiver", recv, n.ID)
		}
		return p.s.Put(p.ctx, temporalIndexJavaMethod, "", "file", n.FilePath, n.ID)
	}); err != nil {
		return err
	}

	// Visit the same annotations again in native order to retain each exact
	// causal recorder without serializing annotation edges into the index.
	return p.visitTemporalAnnotations(func(e *graph.Edge) error {
		ifaceRole, _ := temporalRoleForJavaAnnotation(e.To)
		if ifaceRole == "" || e.From == "" {
			return nil
		}
		iface, err := p.s.Node(p.ctx, e.From)
		if err != nil || iface == nil || iface.Kind != graph.KindInterface {
			return err
		}
		return p.propagateTemporalJava(e, iface, ifaceRole, appendRow)
	})
}

func (p *rebuildTemporal) propagateTemporalJava(e *graph.Edge, iface *graph.Node, ifaceRole string, appendRow func(kind, name, id string) error) error {
	role := "activity"
	if ifaceRole == "workflow_interface" {
		role = "workflow"
	}
	args, _ := e.Meta["args"].(string)
	prefix := javaAnnotationStringArg(args, "namePrefix")
	stampMethod := func(m *graph.Node) error {
		canonical := iface.Name
		if role == "activity" {
			canonical = prefix + capitalizeASCII(m.Name)
		}
		if err := p.stampTemporalJava(e, m, role, canonical); err != nil {
			return err
		}
		return appendRow(role, canonical, m.ID)
	}
	if err := p.visitTemporalJavaMethods("file", iface.FilePath, func(m *graph.Node) error {
		if m.StartLine < iface.StartLine || (iface.EndLine > 0 && m.StartLine > iface.EndLine) {
			return nil
		}
		if err := p.s.Put(p.ctx, temporalIndexJavaMember, "", iface.ID, m.Name); err != nil {
			return err
		}
		if cur, _ := m.Meta["temporal_role"].(string); cur != "" {
			return nil // Already has an explicit or earlier interface stamp.
		}
		return stampMethod(m)
	}); err != nil {
		return err
	}

	// Native prefetch appends the incoming implementors once per tag for
	// this interface, then consumes the resulting list for EACH tag.
	return p.s.Visit(p.ctx, temporalIndexJavaTag, []string{iface.ID}, func(_ []string, _ string) error {
		return p.s.Incoming(p.ctx, iface.ID, graph.EdgeImplements, func(link *graph.Edge) error {
			if link.From == "" {
				return nil
			}
			impl, err := p.s.Node(p.ctx, link.From)
			if err != nil || impl == nil {
				return err
			}
			visitReceiver := func(recv string) error {
				return p.visitTemporalJavaMethods("receiver", recv, func(m *graph.Node) error {
					_, member, err := p.s.Get(p.ctx, temporalIndexJavaMember, iface.ID, m.Name)
					if err != nil || !member {
						return err
					}
					return stampMethod(m) // Implementors do not protect explicit roles.
				})
			}
			if err := visitReceiver(impl.Name); err != nil {
				return err
			}
			// Native also accepts every receiver that is a ::-delimited
			// suffix of the implementor ID. The receiver groups are disjoint.
			for rest := impl.ID; ; {
				i := strings.Index(rest, "::")
				if i < 0 {
					return nil
				}
				recv := rest[i+2:]
				if recv != "" && recv != impl.Name {
					if err := visitReceiver(recv); err != nil {
						return err
					}
				}
				rest = rest[i+1:]
			}
		})
	})
}

func (p *rebuildTemporal) visitTemporalJavaMethods(by, key string, f func(*graph.Node) error) error {
	return p.s.Visit(p.ctx, temporalIndexJavaMethod, []string{by, key}, func(fields []string, _ string) error {
		n, err := p.temporalIndexNode(fields[2])
		if err != nil {
			return err
		}
		return f(n)
	})
}

func (p *rebuildTemporal) temporalIndexNode(id string) (*graph.Node, error) {
	n, err := p.s.Node(p.ctx, id)
	if err == nil && n == nil {
		return nil, fmt.Errorf("temporal index node %q missing", id)
	}
	return n, err
}

func (p *rebuildTemporal) lookup(kind, name, callerRepo, callerLang string) (id, origin string, confidence float64) {
	id = p.lookupTemporalIndex(kind+"::"+name, callerRepo, func(n *graph.Node) bool {
		return callerLang == "" || n.Language == callerLang
	})
	if id == "" {
		return "", "", 0
	}
	return id, graph.OriginASTResolved, 0.9
}

func (p *rebuildTemporal) lookupConvention(kind, name, callerRepo, callerLang string) string {
	suffix := "Activity"
	if kind == "workflow" {
		suffix = "Workflow"
	}
	if !strings.HasSuffix(name, suffix) || (callerLang != "" && callerLang != "go") {
		return ""
	}
	return p.lookupTemporalGoName(name, callerRepo, nil)
}

func (p *rebuildTemporal) lookupExactSig(kind, name, callerRepo string) string {
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return ""
	}
	return p.lookupTemporalGoName(name, callerRepo, func(n *graph.Node) bool {
		return signatureMatchesKind(n, kind)
	})
}

func (p *rebuildTemporal) lookupCrossLang(kind, name, callerLang string) (id string, ok bool) {
	if callerLang == "" {
		return "", false
	}
	// Unlike the registered same-language lookup, native cross-language
	// sees all other-language rows without the cross-repo test filter.
	id = p.lookupTemporalIndex(kind+"::"+name, "", func(n *graph.Node) bool {
		return n.Language != callerLang
	})
	return id, id != ""
}

func (p *rebuildTemporal) lookupTemporalIndex(key, callerRepo string, eligible func(*graph.Node) bool) string {
	return p.pickTemporalCandidate(callerRepo, eligible, func(yield func(*graph.Node) error) error {
		return p.s.Visit(p.ctx, temporalIndexRegistered, []string{key}, func(_ []string, id string) error {
			n, err := p.temporalIndexNode(id)
			if err != nil {
				return err
			}
			return yield(n)
		})
	})
}

func (p *rebuildTemporal) lookupTemporalGoName(name, callerRepo string, eligible func(*graph.Node) bool) string {
	return p.pickTemporalCandidate(callerRepo, eligible, func(yield func(*graph.Node) error) error {
		return p.s.View().VisitNameCandidates(p.ctx, name, []string{"go"}, func(n *graph.Node) error {
			if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
				return nil
			}
			return yield(n)
		})
	})
}

func (p *rebuildTemporal) pickTemporalCandidate(callerRepo string, eligible func(*graph.Node) bool,
	visit func(func(*graph.Node) error) error,
) string {
	if p.err != nil {
		return ""
	}
	var sameRepo, all rebuildTemporalUnique
	p.err = visit(func(n *graph.Node) error {
		if !isCrossRepoTestStub(n, callerRepo) && (eligible == nil || eligible(n)) {
			all.add(n.ID)
			if callerRepo != "" && n.RepoPrefix == callerRepo {
				sameRepo.add(n.ID)
			}
		}
		return nil
	})
	if id := sameRepo.winner(); id != "" {
		return id
	}
	return all.winner()
}
