package store_pebble

import (
	"encoding/binary"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// The inactive receipt embeds unmodified v2 bytes. Decoding the base has
// v2 semantics (including present-empty slices decoding as nil).
const contractParseFactsFormat byte = 4

// EncodeContractParseFacts builds a candidate contract receipt. Active v2
// writers still use EncodeParseFacts. The outer parse-store envelope owns the
// content/recipe key and payload digest; the receipt embeds the exact v2 bytes.
func EncodeContractParseFacts(base ParseFacts, facts *contracts.FileFacts) ([]byte, error) {
	if err := checkContractParseFacts(base, facts); err != nil {
		return nil, err
	}
	v2, err := EncodeParseFacts(base)
	if err != nil {
		return nil, err
	}
	return encodeContractFactSuffix(v2, facts)
}

func encodeContractFactSuffix(v2 []byte, facts *contracts.FileFacts) ([]byte, error) {
	e := contractFactsEncoder{buf: appendBytes([]byte{contractParseFactsFormat}, v2)}
	e.boolean(facts.ScanSkipped)
	writeFactSlice(&e, facts.Records, writeFactRecord)
	writeFactSlice(&e, facts.Questions, writeFactQuestion)
	writeFactMap(&e, facts.Shapes, writeFactShape)
	writeFactMap(&e, facts.Handlers, writeFactHandler)
	writeFactMap(&e, facts.LaravelHandlerClasses, writeFactString)
	writeFactRouter(&e, facts.Router)
	writeFactSlice(&e, facts.ConfigKeys, writeFactConfigKey)
	if e.err != nil {
		return nil, e.err
	}
	return e.buf, nil
}

// DecodeContractParseFacts requires a complete contract receipt; v2 is not a
// built-empty contract result. factBytes is the exact encoded contract suffix.
func DecodeContractParseFacts(raw []byte) (base ParseFacts, facts *contracts.FileFacts, factBytes int, err error) {
	if len(raw) == 0 || raw[0] != contractParseFactsFormat {
		return ParseFacts{}, nil, 0, invalid("contract parse facts format")
	}
	d := recordCursor{buf: raw[1:]}
	v2 := d.bytes()
	if len(v2) == 0 || v2[0] != parseFactsWithLanguageFormat {
		return ParseFacts{}, nil, 0, invalid("contract receipt requires v2 base facts")
	}
	base, err = DecodeParseFacts(v2)
	if err != nil {
		return ParseFacts{}, nil, 0, err
	}
	factBytes = len(d.buf)
	facts = &contracts.FileFacts{
		ScanSkipped:           d.boolean(),
		Records:               readFactSlice(&d, readFactRecord),
		Questions:             readFactSlice(&d, readFactQuestion),
		Shapes:                readFactMap(&d, readFactShape),
		Handlers:              readFactMap(&d, readFactHandler),
		LaravelHandlerClasses: readFactMap(&d, (*recordCursor).str),
		Router:                readFactRouter(&d),
		ConfigKeys:            readFactSlice(&d, readFactConfigKey),
	}
	if err := d.finish(); err != nil {
		return ParseFacts{}, nil, 0, err
	}
	if err := checkContractParseFacts(base, facts); err != nil {
		return ParseFacts{}, nil, 0, err
	}
	return base, facts, factBytes, nil
}

func checkContractParseFacts(base ParseFacts, f *contracts.FileFacts) error {
	if f == nil || base.EffectiveLanguage == "" || f.Shapes == nil {
		return invalid("missing contract facts, shapes, or effective language")
	}
	if f.Handlers == nil {
		return invalid("missing contract handlers")
	}
	if f.ScanSkipped && (len(f.Records) != 0 || len(f.Questions) != 0) {
		return invalid("contract records on a skipped scan")
	}
	path := pathkey.Normalize(base.File.FilePath)
	var classKeys map[string]struct{}
	for _, c := range f.Records {
		if c.ID == "" || !validFactRecord(c, path) {
			return invalid("contract record identity")
		}
		if key, _ := contracts.LaravelHandlerClassKey(c); key != "" {
			if _, ok := f.LaravelHandlerClasses[key]; !ok {
				return invalid("missing or invalid Laravel handler class answer")
			}
			if classKeys == nil {
				classKeys = make(map[string]struct{})
			}
			classKeys[key] = struct{}{}
		}
	}
	if len(f.LaravelHandlerClasses) != len(classKeys) {
		return invalid("unattributed Laravel handler class answers")
	}
	pos := 0
	for _, q := range f.Questions {
		if q.Position < pos || q.Position > len(f.Records) || q.Record.ID != "" || !validFactRecord(q.Record, path) || len(q.Expression.Terms) == 0 {
			return invalid("contract question identity or order")
		}
		pos = q.Position
		switch q.Kind {
		case contracts.EndpointGoRoute, contracts.EndpointTopic, contracts.EndpointHTTPClient:
		default:
			return invalid("contract question kind")
		}
		for _, term := range q.Expression.Terms {
			if (term.Name == "") == (term.Literal == "") {
				return invalid("contract question term")
			}
		}
	}
	shapes := make(map[string]struct{})
	handlers := make(map[string]struct{})
	for _, n := range base.Nodes {
		if n == nil {
			return invalid("nil base node in contract receipt")
		}
		switch n.Kind {
		case graph.KindType, graph.KindInterface:
			if _, ok := f.Shapes[n.ID]; !ok {
				return invalid("missing contract type shape answer")
			}
			shapes[n.ID] = struct{}{}
		case graph.KindFunction, graph.KindMethod:
			h, ok := f.Handlers[n.ID]
			goPath := strings.HasSuffix(n.FilePath, ".go")
			if !ok || h.Regex == nil || (!goPath && h.AST == nil) {
				return invalid("missing contract handler answer")
			}
			// A nil Go AST is an explicit native no-tree answer. A present
			// tree answer has a bindings map even when no bindings were found.
			if goPath && (h.AST == nil) != (h.Bindings == nil) {
				return invalid("incomplete Go contract handler tree answer")
			}
			if raw, ok := h.AST["status_codes"]; ok {
				if _, typed := raw.([]int); !typed {
					return invalid("contract handler status codes type")
				}
			}
			if raw, ok := h.AST["query_params"]; ok {
				if _, typed := raw.([]string); !typed {
					return invalid("contract handler query params type")
				}
			}
			handlers[n.ID] = struct{}{}
		}
	}
	if len(f.Shapes) != len(shapes) || len(f.Handlers) != len(handlers) {
		return invalid("unattributed contract shape or handler answers")
	}
	for _, n := range f.ConfigKeys {
		if err := checkParseNode(n); err != nil {
			return err
		}
		redacted, _ := n.Meta["value_redacted"].(bool)
		if n.Kind != graph.KindConfigKey || n.FilePath != path || !redacted {
			return invalid("contract config key identity or redaction")
		}
		for key, value := range n.Meta {
			switch key {
			case "value_redacted":
			case "source", "raw_key", "profile":
				if _, ok := value.(string); !ok {
					return invalid("contract config key metadata")
				}
			default:
				return invalid("contract config key metadata")
			}
		}
	}
	if f.Router == nil && contracts.RouterFactsRequired(path, base.EffectiveLanguage, f.Records, f.Questions) {
		return invalid("missing contract router facts")
	}
	if f.Router != nil {
		last := 0
		if f.Router.LineCount < 0 {
			return invalid("contract router line count")
		}
		for _, line := range f.Router.Lines {
			if line.Line <= last || line.Line > f.Router.LineCount {
				return invalid("contract router line order")
			}
			last = line.Line
		}
	}
	return nil
}

func validFactRecord(c contracts.Contract, path string) bool {
	// Endpoints and enrichment update metadata. Owner scope belongs to replay,
	// not this portable receipt. Non-finite confidence is not a valid record.
	return c.FilePath == path && c.Type != "" && (c.Role == contracts.RoleProvider || c.Role == contracts.RoleConsumer) && c.Meta != nil &&
		c.RepoPrefix == "" && c.WorkspaceID == "" && c.ProjectID == "" && !math.IsNaN(c.Confidence) && !math.IsInf(c.Confidence, 0)
}

// Collections encode nil as -1 and present empty as zero. A map's sorted keys
// and a slice's order are part of the format. Each item occupies at least a byte.
type contractFactsEncoder struct {
	buf []byte
	err error
}

func (e *contractFactsEncoder) strings(values ...string) {
	for _, s := range values {
		e.buf = appendMetaString(e.buf, s)
	}
}

func (e *contractFactsEncoder) integer(v int) {
	e.buf = binary.AppendVarint(e.buf, int64(v))
}

func (e *contractFactsEncoder) boolean(v bool) {
	e.buf = appendBool(e.buf, v)
}

func (e *contractFactsEncoder) count(n int, nilValue bool) {
	if nilValue {
		n = -1
	}
	e.integer(n)
}

func factCount(d *recordCursor) int {
	n := d.integer()
	if d.err != nil {
		return -1
	}
	if n < -1 || n > len(d.buf) {
		d.err = invalid("contract fact count")
		return -1
	}
	return n
}

func writeFactSlice[T any](e *contractFactsEncoder, v []T, write func(*contractFactsEncoder, T)) {
	e.count(len(v), v == nil)
	for _, item := range v {
		write(e, item)
	}
}

func readFactSlice[T any](d *recordCursor, read func(*recordCursor) T) []T {
	n := factCount(d)
	if n < 0 {
		return nil
	}
	out := make([]T, 0)
	for i := 0; i < n && d.err == nil; i++ {
		out = append(out, read(d))
	}
	return out
}

func writeFactMap[T any](e *contractFactsEncoder, v map[string]T, write func(*contractFactsEncoder, T)) {
	e.count(len(v), v == nil)
	for _, key := range slices.Sorted(maps.Keys(v)) {
		e.strings(key)
		write(e, v[key])
	}
}

func readFactMap[T any](d *recordCursor, read func(*recordCursor) T) map[string]T {
	n := factCount(d)
	if n < 0 {
		return nil
	}
	out := make(map[string]T)
	var previous string
	for i := 0; i < n && d.err == nil; i++ {
		key := d.str()
		if d.err != nil {
			return nil
		}
		if i > 0 && key <= previous {
			d.err = invalid("contract fact map order")
			return nil
		}
		previous = key
		value := read(d)
		if d.err != nil {
			return nil
		}
		out[key] = value
	}
	return out
}

func writeFactString(e *contractFactsEncoder, s string) {
	e.strings(s)
}

func writeFactRecord(e *contractFactsEncoder, c contracts.Contract) {
	e.strings(c.ID, string(c.Type), string(c.Role), c.SymbolID, c.FilePath)
	e.integer(c.Line)
	e.strings(c.RepoPrefix, c.WorkspaceID, c.ProjectID)
	writeFactFloat(e, c.Confidence)
	writeFactMeta(e, c.Meta, 0)
}

func readFactRecord(d *recordCursor) contracts.Contract {
	return contracts.Contract{
		ID:          d.str(),
		Type:        contracts.ContractType(d.str()),
		Role:        contracts.Role(d.str()),
		SymbolID:    d.str(),
		FilePath:    d.str(),
		Line:        d.integer(),
		RepoPrefix:  d.str(),
		WorkspaceID: d.str(),
		ProjectID:   d.str(),
		Confidence:  readFactFloat(d),
		Meta:        readFactMeta(d, 0),
	}
}

func writeFactQuestion(e *contractFactsEncoder, q contracts.EndpointContractQuestion) {
	e.integer(q.Position)
	e.strings(string(q.Kind), q.Method)
	e.boolean(q.StdlibConsumer)
	e.boolean(q.Expression.Dereferenced)
	writeFactSlice(e, q.Expression.Terms, func(e *contractFactsEncoder, term contracts.EndpointTerm) {
		e.strings(term.Name, term.Literal)
	})
	writeFactRecord(e, q.Record)
}

func readFactQuestion(d *recordCursor) contracts.EndpointContractQuestion {
	q := contracts.EndpointContractQuestion{
		Position:       d.integer(),
		Kind:           contracts.EndpointQuestionKind(d.str()),
		Method:         d.str(),
		StdlibConsumer: d.boolean(),
		Expression: contracts.EndpointExpression{
			Dereferenced: d.boolean(),
			Terms: readFactSlice(d, func(d *recordCursor) contracts.EndpointTerm {
				return contracts.EndpointTerm{Name: d.str(), Literal: d.str()}
			}),
		},
	}
	q.Record = readFactRecord(d)
	return q
}

func writeFactShape(e *contractFactsEncoder, shape *contracts.Shape) {
	e.boolean(shape != nil)
	if shape == nil {
		return
	}
	e.strings(shape.Kind)
	writeFactSlice(e, shape.Fields, func(e *contractFactsEncoder, field contracts.ShapeField) {
		e.strings(field.Name, field.Type, field.JSONTag, field.Comment)
		e.boolean(field.Required)
		e.boolean(field.Repeated)
	})
	writeFactSlice(e, shape.Notes, writeFactString)
}

func readFactShape(d *recordCursor) *contracts.Shape {
	if !d.boolean() {
		return nil
	}
	return &contracts.Shape{
		Kind: d.str(),
		Fields: readFactSlice(d, func(d *recordCursor) contracts.ShapeField {
			return contracts.ShapeField{
				Name:     d.str(),
				Type:     d.str(),
				JSONTag:  d.str(),
				Comment:  d.str(),
				Required: d.boolean(),
				Repeated: d.boolean(),
			}
		}),
		Notes: readFactSlice(d, (*recordCursor).str),
	}
}

func writeFactHandler(e *contractFactsEncoder, h contracts.HandlerFacts) {
	writeFactMeta(e, h.Regex, 0)
	writeFactMeta(e, h.AST, 0)
	writeFactMap(e, h.Bindings, func(e *contractFactsEncoder, b contracts.Binding) {
		e.strings(string(b.Kind), b.TypeID, b.CallExpr, b.RawExpr, string(b.Origin))
		e.boolean(b.Repeated)
		e.boolean(b.Pointer)
		e.integer(b.Line)
	})
}

func readFactHandler(d *recordCursor) contracts.HandlerFacts {
	return contracts.HandlerFacts{
		Regex: readFactMeta(d, 0),
		AST:   readFactMeta(d, 0),
		Bindings: readFactMap(d, func(d *recordCursor) contracts.Binding {
			return contracts.Binding{
				Kind:     contracts.BindingKind(d.str()),
				TypeID:   d.str(),
				CallExpr: d.str(),
				RawExpr:  d.str(),
				Origin:   contracts.Origin(d.str()),
				Repeated: d.boolean(),
				Pointer:  d.boolean(),
				Line:     d.integer(),
			}
		}),
	}
}

func writeFactRouter(e *contractFactsEncoder, router *contracts.RouterFacts) {
	e.boolean(router != nil)
	if router == nil {
		return
	}
	for _, v := range []map[string]string{router.SelfPrefix, router.MountPrefix, router.MountParent, router.ModulePrefix, router.ControllerModule} {
		writeFactMap(e, v, writeFactString)
	}
	e.strings(router.ControllerClass)
	e.integer(router.LineCount)
	writeFactSlice(e, router.Lines, func(e *contractFactsEncoder, line contracts.RouterLineFacts) {
		e.integer(line.Line)
		writeFactMap(e, line.Receivers, writeFactString)
		e.strings(line.NestPrefix, line.SpringPrefix, line.RailsPrefix, line.LaravelPrefix, line.VaporPrefix)
	})
}

func readFactRouter(d *recordCursor) *contracts.RouterFacts {
	if !d.boolean() {
		return nil
	}
	return &contracts.RouterFacts{
		SelfPrefix:       readFactMap(d, (*recordCursor).str),
		MountPrefix:      readFactMap(d, (*recordCursor).str),
		MountParent:      readFactMap(d, (*recordCursor).str),
		ModulePrefix:     readFactMap(d, (*recordCursor).str),
		ControllerModule: readFactMap(d, (*recordCursor).str),
		ControllerClass:  d.str(),
		LineCount:        d.integer(),
		Lines: readFactSlice(d, func(d *recordCursor) contracts.RouterLineFacts {
			return contracts.RouterLineFacts{
				Line:          d.integer(),
				Receivers:     readFactMap(d, (*recordCursor).str),
				NestPrefix:    d.str(),
				SpringPrefix:  d.str(),
				RailsPrefix:   d.str(),
				LaravelPrefix: d.str(),
				VaporPrefix:   d.str(),
			}
		}),
	}
}

func writeFactConfigKey(e *contractFactsEncoder, node *graph.Node) {
	if e.err != nil {
		return
	}
	row, err := encodeNode(node)
	if err != nil {
		e.err = err
		return
	}
	e.buf = appendBytes(e.buf, row)
}

func readFactConfigKey(d *recordCursor) *graph.Node {
	row := d.bytes()
	if d.err != nil {
		return nil
	}
	node, err := decodeNode(row)
	if err != nil {
		d.err = err
	}
	return node
}
