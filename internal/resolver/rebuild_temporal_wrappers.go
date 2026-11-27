package resolver

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/zzet/gortex/internal/graph"
)

func (p *rebuildTemporal) wrappers() (rounds int, capped bool, err error) {
	const maxRounds = 16
	var wrappers uint64
	for rounds < maxRounds {
		rounds++
		if err := p.indexWrappers(&wrappers); err != nil {
			return rounds, false, err
		}
		if wrappers == 0 {
			return rounds, false, nil
		}
		added, err := p.wrapperSweep(temporalOrdinal(uint64(rounds)))
		if err != nil || !added {
			return rounds, false, err
		}
	}
	return rounds, true, nil
}

// W holds the first valid wrapper per source. N preserves their insertion
// order within each bare name. Both survive across sweeps: seed rows are
// append-only and their parameter and name facts do not change in this phase.
func (p *rebuildTemporal) indexWrappers(ordinal *uint64) error {
	return p.visitWrapperSeeds(func(e *graph.Edge) error {
		if e == nil || e.From == "" || e.Meta["via"] != "temporal.stub" {
			return nil
		}
		param, _ := e.Meta["temporal_name_param"].(string)
		kind, _ := e.Meta["temporal_kind"].(string)
		if param == "" || kind == "" {
			return nil
		}
		if _, seen, err := p.s.Get(p.ctx, 'W', e.From); err != nil || seen {
			return err
		}
		pn, err := p.s.Node(p.ctx, e.From+"#param:"+param)
		if err != nil || pn == nil {
			return err
		}
		pos, ok := metaIntValue(pn.Meta["position"])
		if !ok {
			return nil
		}
		wn, err := p.s.Node(p.ctx, e.From)
		if err != nil {
			return err
		}
		name := ""
		if wn != nil {
			name = wn.Name
		}
		if err := p.s.Put(p.ctx, 'W', kind, e.From); err != nil {
			return err
		}
		if err := p.s.Put(p.ctx, 'W', strconv.Itoa(pos), e.From, "position"); err != nil {
			return err
		}
		if err := p.s.Put(p.ctx, 'W', name, e.From, "name"); err != nil {
			return err
		}
		*ordinal = *ordinal + 1
		if name == "" {
			return nil
		}
		return p.s.Put(p.ctx, 'N', kind, name, temporalOrdinal(*ordinal), e.From)
	})
}

func (p *rebuildTemporal) wrapperSweep(round string) (bool, error) {
	var pending uint64
	err := p.s.VisitWrapperCalls(p.ctx, func(call *graph.Edge) error {
		// A staged wrapper shadows the full base identity in the mux
		// caller scan. It never has arg_names, so it never enters this join.
		if _, staged, err := p.s.Get(p.ctx, 'H', temporalWrapperKey(call)...); err != nil || staged {
			return err
		}
		kind, exact, err := p.s.Get(p.ctx, 'W', call.To)
		if err != nil {
			return err
		}
		if exact {
			// An exact entry suppresses fallback even if it emits nothing.
			name, _, err := p.s.Get(p.ctx, 'W', call.To, "name")
			if err != nil {
				return err
			}
			return p.stageWrapperCall(call, call.To, kind, name, round, &pending)
		}
		callee, _ := call.Meta["callee"].(string)
		if callee == "" {
			return nil
		}
		return p.s.Visit(p.ctx, 'N', []string{callee}, func(fields []string, kind string) error {
			return p.stageWrapperCall(call, fields[2], kind, callee, round, &pending)
		})
	})
	if err != nil || pending == 0 {
		return false, err
	}

	// Q preserves caller order, then posting order, without collapsing any
	// proposals. Only after the scan ends does live dedup see each addition.
	added := false
	err = p.s.Visit(p.ctx, 'Q', []string{round}, func(fields []string, _ string) error {
		accepted, err := p.acceptWrapperCall(fields)
		added = added || accepted
		return err
	})
	return added, err
}

func (p *rebuildTemporal) stageWrapperCall(call *graph.Edge, id, kind, wrapperName, round string, pending *uint64) error {
	if call.From == id {
		return nil
	}
	stored, _, err := p.s.Get(p.ctx, 'W', id, "position")
	if err != nil {
		return err
	}
	pos, err := strconv.Atoi(stored)
	if err != nil {
		return fmt.Errorf("temporal wrapper position: %w", err)
	}
	name := argNameAt(call, pos)
	if name == "" {
		return nil
	}
	pn, err := p.s.Node(p.ctx, call.From+"#param:"+name)
	if err != nil {
		return err
	}
	fwd := ""
	if pn != nil && pn.Kind == graph.KindParam {
		if _, ok := metaIntValue(pn.Meta["position"]); ok {
			fwd = name
		}
	}
	*pending = *pending + 1
	return p.s.Put(p.ctx, 'Q', "", round, temporalOrdinal(*pending),
		call.From, call.To, call.FilePath, strconv.Itoa(call.Line), kind, name, wrapperName, fwd)
}

func (p *rebuildTemporal) acceptWrapperCall(fields []string) (bool, error) {
	line, err := strconv.Atoi(fields[5])
	if err != nil {
		return false, fmt.Errorf("temporal wrapper call line: %w", err)
	}
	key := graph.EdgeIdentity{From: fields[2], To: fields[3], Kind: graph.EdgeCalls, FilePath: fields[4], Line: line}
	trigger, err := p.s.Edge(p.ctx, key)
	if err != nil {
		return false, err
	}
	if trigger == nil {
		return false, fmt.Errorf("temporal wrapper caller missing at %v", key)
	}
	kind, name, wrapperName, fwd := fields[6], fields[7], fields[8], fields[9]
	meta := map[string]any{
		"via":                  "temporal.stub",
		"temporal_kind":        kind,
		"temporal_name":        name,
		"temporal_via_wrapper": wrapperName,
	}
	if fwd != "" {
		meta["temporal_name_param"] = fwd
	}
	stub := &graph.Edge{
		From: key.From, To: temporalStubPlaceholder(kind, name),
		Kind: graph.EdgeCalls, FilePath: key.FilePath, Line: key.Line, Meta: meta,
	}
	return p.queueWrapper(trigger, stub)
}

func temporalWrapperKey(e *graph.Edge) []string {
	return []string{e.From, e.To, e.FilePath, strconv.Itoa(e.Line)}
}

func rebuildWrapperStub(fields []string) (*graph.Edge, error) {
	line, err := strconv.Atoi(fields[4])
	if err != nil {
		return nil, err
	}
	meta := map[string]any{
		"via":                  "temporal.stub",
		"temporal_kind":        fields[5],
		"temporal_name":        fields[6],
		"temporal_via_wrapper": fields[7],
	}
	if fields[8] != "" {
		meta["temporal_name_param"] = fields[8]
	}
	return &graph.Edge{From: fields[1], To: fields[2], Kind: graph.EdgeCalls, FilePath: fields[3], Line: line, Meta: meta}, nil
}

// B preserves all accepted wrapper seeds. H indexes a staged key by its
// first slot. S points from that slot to the last payload when overwritten.
func (p *rebuildTemporal) visitWrapperSeeds(yield func(*graph.Edge) error) error {
	if err := p.visitTemporalCalls(yield); err != nil {
		return err
	}
	return p.s.Visit(p.ctx, 'B', nil, func(fields []string, _ string) error {
		e, err := rebuildWrapperStub(fields)
		if err != nil {
			return err
		}
		return yield(e)
	})
}

func (p *rebuildTemporal) wrapperSlot(slot string, yield func(*graph.Edge) error) error {
	last, found, err := p.s.Get(p.ctx, 'S', slot)
	if err != nil {
		return err
	}
	if !found {
		last = slot
	}
	return p.s.Visit(p.ctx, 'B', []string{last}, func(fields []string, _ string) error {
		e, err := rebuildWrapperStub(fields)
		if err != nil {
			return err
		}
		return yield(e)
	})
}

func (p *rebuildTemporal) visitNewWrappers(yield func(*graph.Edge) error) error {
	return p.s.Visit(p.ctx, 'B', nil, func(fields []string, _ string) error {
		slot, found, err := p.s.Get(p.ctx, 'H', fields[1:5]...)
		if err != nil || !found || slot != fields[0] {
			return err
		}
		return p.wrapperSlot(slot, yield)
	})
}

func (p *rebuildTemporal) queueWrapper(trigger, stub *graph.Edge) (bool, error) {
	kind, _ := stub.Meta["temporal_kind"].(string)
	name, _ := stub.Meta["temporal_name"].(string)
	var duplicate bool
	check := func(e *graph.Edge) {
		if e.Meta["via"] == "temporal.stub" && e.Meta["temporal_kind"] == kind && e.Meta["temporal_name"] == name {
			duplicate = true
		}
	}
	if err := p.s.Visit(p.ctx, 'H', []string{stub.From}, func(_ []string, slot string) error {
		if duplicate {
			return nil
		}
		return p.wrapperSlot(slot, func(e *graph.Edge) error {
			check(e)
			return nil
		})
	}); err != nil || duplicate {
		return false, err
	}
	if err := p.s.Outgoing(p.ctx, stub.From, func(e *graph.Edge) error {
		if duplicate || e.Meta["via"] != "temporal.stub" {
			return nil
		}
		if e.Kind == graph.EdgeCalls {
			if _, staged, err := p.s.Get(p.ctx, 'H', temporalWrapperKey(e)...); err != nil || staged {
				return err
			}
		}
		check(e)
		return nil
	}); err != nil || duplicate {
		return false, err
	}
	proof := p.s.WrapperProof(stub)
	if err := proof.WantRecorder(p.ctx, trigger); err != nil {
		return false, err
	}
	if err := proof.WantNode(p.ctx, stub.From); err != nil {
		return false, err
	}
	key := temporalWrapperKey(stub)
	slot, found, err := p.s.Get(p.ctx, 'H', key...)
	if err != nil {
		return false, err
	}
	p.wrappersAdded++
	ordinal := temporalOrdinal(p.wrappersAdded)
	if found {
		err = p.s.Put(p.ctx, 'S', ordinal, slot)
	} else {
		err = p.s.Put(p.ctx, 'H', ordinal, key...)
	}
	if err != nil {
		return false, err
	}
	wrapperName, _ := stub.Meta["temporal_via_wrapper"].(string)
	fwd, _ := stub.Meta["temporal_name_param"].(string)
	err = p.s.Put(p.ctx, 'B', "", slices.Concat([]string{ordinal}, key, []string{kind, name, wrapperName, fwd})...)
	return err == nil, err
}

func executorDispatchKey(e *graph.Edge) string {
	if e == nil || e.Meta["via"] != "temporal.stub" {
		return ""
	}
	field, _ := e.Meta["temporal_name_field"].(string)
	rtype, _ := e.Meta["temporal_recv_type"].(string)
	kind, _ := e.Meta["temporal_kind"].(string)
	if field == "" || rtype == "" || kind == "" {
		return ""
	}
	return rtype + "::" + field
}

// X reduces construction markers for demanded base dispatch keys. Empty
// means not seen, "!" means ambiguous, and "=" prefixes a unique literal.
func (p *rebuildTemporal) executorFields() error {
	demanded := false
	err := p.visitTemporalCalls(func(e *graph.Edge) error {
		key := executorDispatchKey(e)
		if key == "" {
			return nil
		}
		demanded = true
		return p.s.Put(p.ctx, 'X', "", key)
	})
	if err != nil || !demanded {
		return err
	}
	return p.visitTemporalCalls(func(e *graph.Edge) error {
		if e == nil || e.From == "" || e.Meta["via"] != "temporal.executor-field" {
			return nil
		}
		rtype, _ := e.Meta["executor_type"].(string)
		field, _ := e.Meta["executor_field"].(string)
		value, _ := e.Meta["executor_value"].(string)
		if rtype == "" || field == "" || value == "" {
			return nil
		}
		key := rtype + "::" + field
		prior, found, err := p.s.Get(p.ctx, 'X', key)
		next := "=" + value
		if err != nil || !found || prior == "!" || prior == next {
			return err
		}
		if prior != "" {
			next = "!"
		}
		return p.s.Put(p.ctx, 'X', next, key)
	})
}

// applyExecutor changes only the base edge decoded for the main sweep. A
// stationary edge has no write for this patch on the native SQLite path.
func (p *rebuildTemporal) applyExecutor(e *graph.Edge) error {
	key := executorDispatchKey(e)
	if key == "" {
		return nil
	}
	stored, found, err := p.s.Get(p.ctx, 'X', key)
	if err != nil || !found || stored == "" || stored == "!" {
		return err
	}
	e.Meta["temporal_name"] = stored[1:]
	e.Meta["temporal_via_executor"] = true
	return nil
}
