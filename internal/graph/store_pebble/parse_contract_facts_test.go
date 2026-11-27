package store_pebble

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractParseFactsPreserveBytesAndTypes(t *testing.T) {
	const path = "a\xff.go"
	const handler = path + "::H\xfe"
	const dto = path + "::DTO\xf5"
	const empty = path + "::Empty"
	require.False(t, utf8.ValidString(path))
	base := ParseFacts{
		Nodes: []*graph.Node{
			{ID: handler, Kind: graph.KindMethod, Name: "H\xfe", FilePath: path},
			{ID: dto, Kind: graph.KindType, Name: "DTO\xf5", FilePath: path},
			{ID: empty, Kind: graph.KindInterface, Name: "Empty", FilePath: path},
		},
		Edges:             []*graph.Edge{{From: handler, To: dto, Kind: graph.EdgeReferences, FilePath: path, Line: 8}},
		File:              graph.FileMetaRow{FilePath: path, ContentHash: "base", Size: 200, NodeCount: 3, Errors: "[]"},
		Constants:         []graph.ConstantValueRow{{NodeID: path + "::VALUE", FilePath: path, Value: "\xff"}},
		Shingles:          map[string][]uint64{handler: {1, 2}},
		Symbols:           []graph.SymbolFTSItem{{NodeID: handler, Tokens: "test"}},
		EffectiveLanguage: "go",
	}
	provider := contracts.Contract{
		ID: "http::GET::/\xff", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		SymbolID: handler, FilePath: path, Line: 8, Confidence: math.Float64frombits(0x3fe3333333333334),
		Meta: map[string]any{
			"status_codes": []int{203, 200}, "nil_codes": []int(nil), "empty_codes": []int{},
			"response_envelope": []map[string]any{nil, {}, {"type": dto, "note": "\xf5"}},
			"guards":            []map[string]string{{"field": "\xfe", "value": "x"}, nil, {}},
			"nested":            []any{nil, []string(nil), []string{}, map[string]any(nil), map[string]any{}, int64(-8), uint64(9)},
		},
	}
	question := contracts.EndpointContractQuestion{
		Position: 1, Kind: contracts.EndpointGoRoute, Method: "GET",
		Expression: contracts.EndpointExpression{Dereferenced: true, Terms: []contracts.EndpointTerm{{Name: "MISS\xff"}, {Literal: "/fallback"}}},
		Record:     contracts.Contract{FilePath: path, SymbolID: handler, Type: contracts.ContractHTTP, Role: contracts.RoleProvider, Meta: map[string]any{}},
	}
	facts := &contracts.FileFacts{
		Records:   []contracts.Contract{provider},
		Questions: []contracts.EndpointContractQuestion{question},
		Shapes: map[string]*contracts.Shape{
			dto:   {Kind: "struct", Fields: []contracts.ShapeField{{Name: "field\xff", Type: dto, JSONTag: "f", Required: true, Repeated: true, Comment: "note"}}, Notes: []string{}},
			empty: nil,
		},
		Handlers: map[string]contracts.HandlerFacts{handler: {
			Regex: map[string]any{}, AST: map[string]any{"query_params": []string{"q"}, "status_codes": []int{200}},
			Bindings: map[string]contracts.Binding{"items": {
				Kind: contracts.BindingMethodCall, TypeID: dto, Repeated: true, Pointer: true,
				CallExpr: "h.fetch", RawExpr: "h.fetch()", Line: 7, Origin: contracts.OriginASTResolved,
			}},
		}},
		Router: &contracts.RouterFacts{
			SelfPrefix: map[string]string{"r": "/\xff"}, MountPrefix: map[string]string{}, ModulePrefix: map[string]string{"m": "/module"},
			MountParent: map[string]string{"r": "app"}, ControllerModule: map[string]string{"Controller": "m"}, ControllerClass: "Controller", LineCount: 9,
			Lines: []contracts.RouterLineFacts{{Line: 1, Receivers: map[string]string{"r": "/api"}, NestPrefix: "n", SpringPrefix: "s", RailsPrefix: "r", LaravelPrefix: "l", VaporPrefix: "v"}},
		},
		ConfigKeys: []*graph.Node{{
			ID: "cfg::spring::app.key", Name: "app.key", Kind: graph.KindConfigKey, FilePath: path, StartLine: 1,
			Meta: map[string]any{"source": "spring", "raw_key": "app.key", "value_redacted": true, "profile": "test"},
		}},
	}
	payload, err := EncodeContractParseFacts(base, facts)
	require.NoError(t, err)
	decodedBase, decodedFacts, factBytes, err := DecodeContractParseFacts(payload)
	require.NoError(t, err)
	require.Equal(t, base, decodedBase)
	require.Equal(t, facts, decodedFacts, "types, byte strings, and nil and empty values must survive")
	require.Equal(t, math.Float64bits(provider.Confidence), math.Float64bits(decodedFacts.Records[0].Confidence))
	v2, err := EncodeParseFacts(base)
	require.NoError(t, err)
	prefix := recordCursor{buf: payload[1:]}
	require.Equal(t, v2, prefix.bytes(), "strip the contract suffix without re-encoding to get the exact v2 bytes")
	require.Equal(t, len(prefix.buf), factBytes)
	reencoded, err := EncodeContractParseFacts(decodedBase, decodedFacts)
	require.NoError(t, err)
	require.Equal(t, payload, reencoded)
	delete(decodedFacts.Shapes, empty)
	_, err = EncodeContractParseFacts(decodedBase, decodedFacts)
	require.ErrorIs(t, err, errInvalidRecord, "missing shape is not the same as its explicit nil answer")
	_, err = DecodeParseFacts(payload)
	require.ErrorIs(t, err, errInvalidRecord, "the active reader must not accept a candidate receipt")
	_, _, _, err = DecodeContractParseFacts(v2)
	require.ErrorIs(t, err, errInvalidRecord, "a prior recipe is not a built-empty result")
	for i := 0; i < len(payload); i++ {
		_, _, _, err := DecodeContractParseFacts(payload[:i])
		require.ErrorIs(t, err, errInvalidRecord, "truncated at byte %d", i)
	}
	_, _, _, err = DecodeContractParseFacts(append(bytes.Clone(payload), 0))
	require.ErrorIs(t, err, errInvalidRecord)
}

func TestContractParseFactsEmptyMissingAndInvalid(t *testing.T) {
	base := ParseFacts{File: graph.FileMetaRow{FilePath: "empty.go"}, EffectiveLanguage: "go"}
	complete := contracts.FileFacts{
		Records: []contracts.Contract{}, Shapes: map[string]*contracts.Shape{}, Handlers: map[string]contracts.HandlerFacts{},
		ScanSkipped: true, Router: &contracts.RouterFacts{}, ConfigKeys: []*graph.Node{},
	}
	payload, err := EncodeContractParseFacts(base, &complete)
	require.NoError(t, err)
	_, got, _, err := DecodeContractParseFacts(payload)
	require.NoError(t, err)
	require.Equal(t, &complete, got, "built empty, nil questions, and present empty router differ from missing")

	_, err = EncodeContractParseFacts(base, nil)
	require.ErrorIs(t, err, errInvalidRecord)
	missingShapes := complete
	missingShapes.Shapes = nil
	_, err = EncodeContractParseFacts(base, &missingShapes)
	require.ErrorIs(t, err, errInvalidRecord)
	missingHandlers := complete
	missingHandlers.Handlers = nil
	_, err = EncodeContractParseFacts(base, &missingHandlers)
	require.ErrorIs(t, err, errInvalidRecord)

	switchToHandler := base
	switchToHandler.File.NodeCount = 1
	switchToHandler.Nodes = []*graph.Node{{ID: "empty.go::H", Kind: graph.KindFunction, FilePath: "empty.go"}}
	_, err = EncodeContractParseFacts(switchToHandler, &complete)
	require.ErrorIs(t, err, errInvalidRecord)
	bad := complete
	bad.ScanSkipped = false
	bad.Questions = []contracts.EndpointContractQuestion{{
		Position:   1,
		Kind:       contracts.EndpointGoRoute,
		Expression: contracts.EndpointExpression{Terms: []contracts.EndpointTerm{{Name: "VALUE"}}},
		Record:     contracts.Contract{FilePath: "empty.go", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, Meta: map[string]any{}},
	}}
	_, err = EncodeContractParseFacts(base, &bad)
	require.ErrorIs(t, err, errInvalidRecord)
}

func TestContractParseFactsRefusesLossyMetadata(t *testing.T) {
	base := ParseFacts{File: graph.FileMetaRow{FilePath: "file.go"}, EffectiveLanguage: "go"}
	f := &contracts.FileFacts{
		Shapes: map[string]*contracts.Shape{}, Handlers: map[string]contracts.HandlerFacts{},
		Records: []contracts.Contract{{ID: "http::GET::/items", FilePath: "file.go", Type: contracts.ContractHTTP, Role: contracts.RoleProvider}},
	}
	f.Records[0].Meta = map[string]any{"bad": []byte{0xff}}
	_, err := EncodeContractParseFacts(base, f)
	require.ErrorIs(t, err, errInvalidRecord, "unknown native types must not go through JSON")
	f.Records[0].Meta = map[string]any{"value": nil}
	good, err := EncodeContractParseFacts(base, f)
	require.NoError(t, err)
	needle := appendMetaString(nil, "value")
	pos := bytes.Index(good, needle) + len(needle)
	require.GreaterOrEqual(t, pos, len(needle))
	require.Equal(t, byte(metaTagNil), good[pos])
	good[pos] = metaTagJSON
	_, _, _, err = DecodeContractParseFacts(good)
	require.ErrorIs(t, err, errInvalidRecord)
}

func TestContractParseFactsLaravelHandlerClasses(t *testing.T) {
	const path = "routes.php"
	base := ParseFacts{File: graph.FileMetaRow{FilePath: path}, EffectiveLanguage: "php"}
	f := &contracts.FileFacts{
		Shapes: map[string]*contracts.Shape{}, Handlers: map[string]contracts.HandlerFacts{},
		Records: []contracts.Contract{{
			ID:       "http::GET::/orders",
			FilePath: path,
			Type:     contracts.ContractHTTP,
			Role:     contracts.RoleProvider,
			Line:     4,
			Meta:     map[string]any{"framework": "laravel", "handler_ident": "show", "handler_class": "Orders"},
		}},
		Router: &contracts.RouterFacts{},
	}
	_, err := EncodeContractParseFacts(base, f)
	require.ErrorIs(t, err, errInvalidRecord, "an absent class answer is not built empty")
	f.LaravelHandlerClasses = map[string]string{"4:Orders": "Acme\\Order\xff"}
	wire, err := EncodeContractParseFacts(base, f)
	require.NoError(t, err)
	_, got, _, err := DecodeContractParseFacts(wire)
	require.NoError(t, err)
	require.Equal(t, f, got)

	f.LaravelHandlerClasses["4:Other"] = "Other"
	_, err = EncodeContractParseFacts(base, f)
	require.ErrorIs(t, err, errInvalidRecord, "answer must belong to a registration class and line")
	wire[0] = 3
	_, _, _, err = DecodeContractParseFacts(wire)
	require.ErrorIs(t, err, errInvalidRecord, "the earlier inactive layout does not supply these answers")
}

func TestContractParseFactsRejectsSemanticViolations(t *testing.T) {
	const path = "routes.go"
	const handler = path + "::H"
	base := ParseFacts{
		File:              graph.FileMetaRow{FilePath: path, NodeCount: 1},
		EffectiveLanguage: "go",
		Nodes:             []*graph.Node{{ID: handler, Kind: graph.KindFunction, FilePath: path}},
	}
	newFacts := func() *contracts.FileFacts {
		record := contracts.Contract{
			FilePath: path, Type: contracts.ContractHTTP, Role: contracts.RoleProvider, Meta: map[string]any{},
		}
		q := contracts.EndpointContractQuestion{
			Position:   0,
			Kind:       contracts.EndpointGoRoute,
			Method:     "GET",
			Record:     record,
			Expression: contracts.EndpointExpression{Terms: []contracts.EndpointTerm{{Name: "ROUTE"}}},
		}
		q2 := q
		q2.Record.Meta = map[string]any{}
		q2.Position = 1
		record.Meta = map[string]any{}
		record.ID = "http::GET::/items"
		return &contracts.FileFacts{
			Records:   []contracts.Contract{record},
			Questions: []contracts.EndpointContractQuestion{q, q2},
			Shapes:    map[string]*contracts.Shape{},
			Handlers: map[string]contracts.HandlerFacts{handler: {
				Regex: map[string]any{}, AST: map[string]any{}, Bindings: map[string]contracts.Binding{},
			}},
			Router: &contracts.RouterFacts{LineCount: 2, Lines: []contracts.RouterLineFacts{{Line: 1}, {Line: 2}}},
			ConfigKeys: []*graph.Node{{
				ID:       "cfg::spring::app.key",
				Kind:     graph.KindConfigKey,
				FilePath: path,
				Meta:     map[string]any{"source": "spring", "raw_key": "app.key", "profile": "test", "value_redacted": true},
			}},
		}
	}
	good, err := EncodeContractParseFacts(base, newFacts())
	require.NoError(t, err)
	_, _, _, err = DecodeContractParseFacts(good)
	require.NoError(t, err)

	for _, tt := range []struct {
		name, reason string
		invalidate   func(*contracts.FileFacts)
	}{
		{"records on skipped scan", "contract records on a skipped scan", func(f *contracts.FileFacts) {
			f.Questions = nil
			f.ScanSkipped = true
		}},
		{"record without ID", "contract record identity", func(f *contracts.FileFacts) { f.Records[0].ID = "" }},
		{"record outside file", "contract record identity", func(f *contracts.FileFacts) { f.Records[0].FilePath = "other.go" }},
		{"record without type", "contract record identity", func(f *contracts.FileFacts) { f.Records[0].Type = "" }},
		{"record with unknown role", "contract record identity", func(f *contracts.FileFacts) { f.Records[0].Role = "unknown" }},
		{"record with NaN confidence", "contract record identity", func(f *contracts.FileFacts) { f.Records[0].Confidence = math.NaN() }},
		{"record with infinite confidence", "contract record identity", func(f *contracts.FileFacts) { f.Records[0].Confidence = math.Inf(1) }},
		{"record with repository scope", "", func(f *contracts.FileFacts) { f.Records[0].RepoPrefix = "repo" }},
		{"record with workspace scope", "", func(f *contracts.FileFacts) { f.Records[0].WorkspaceID = "work" }},
		{"record with project scope", "", func(f *contracts.FileFacts) { f.Records[0].ProjectID = "project" }},
		{"question order goes backwards", "contract question identity or order", func(f *contracts.FileFacts) {
			f.Questions[0], f.Questions[1] = f.Questions[1], f.Questions[0]
		}},
		{"question record with ID", "contract question identity or order", func(f *contracts.FileFacts) {
			f.Questions[0].Record.ID = "http::GET::/items"
		}},
		{"question record without metadata", "contract question identity or order", func(f *contracts.FileFacts) {
			f.Questions[0].Record.Meta = nil
		}},
		{"record without metadata", "contract record identity", func(f *contracts.FileFacts) { f.Records[0].Meta = nil }},
		{"question with repository scope", "", func(f *contracts.FileFacts) { f.Questions[0].Record.RepoPrefix = "repo" }},
		{"question with workspace scope", "", func(f *contracts.FileFacts) { f.Questions[0].Record.WorkspaceID = "work" }},
		{"question with project scope", "", func(f *contracts.FileFacts) { f.Questions[0].Record.ProjectID = "project" }},
		{"unknown question kind", "contract question kind", func(f *contracts.FileFacts) { f.Questions[0].Kind = "unknown" }},
		{"term with name and literal", "contract question term", func(f *contracts.FileFacts) {
			f.Questions[0].Expression.Terms = []contracts.EndpointTerm{{Name: "ROUTE", Literal: "/items"}}
		}},
		{"term with neither name nor literal", "contract question term", func(f *contracts.FileFacts) {
			f.Questions[0].Expression.Terms = []contracts.EndpointTerm{{}}
		}},
		{"Go tree answer without bindings", "", func(f *contracts.FileFacts) {
			h := f.Handlers[handler]
			h.Bindings = nil
			f.Handlers[handler] = h
		}},
		{"Go bindings without tree answer", "", func(f *contracts.FileFacts) {
			h := f.Handlers[handler]
			h.AST = nil
			f.Handlers[handler] = h
		}},
		{"status codes with wrong type", "contract handler status codes type", func(f *contracts.FileFacts) {
			f.Handlers[handler].AST["status_codes"] = []string{"200"}
		}},
		{"query params with wrong type", "contract handler query params type", func(f *contracts.FileFacts) {
			f.Handlers[handler].AST["query_params"] = []int{1}
		}},
		{"unattributed shape", "unattributed contract shape or handler answers", func(f *contracts.FileFacts) {
			f.Shapes[path+"::Other"] = nil
		}},
		{"unattributed handler", "unattributed contract shape or handler answers", func(f *contracts.FileFacts) {
			f.Handlers[path+"::Other"] = f.Handlers[handler]
		}},
		{"config key without ID", "nonportable parse node", func(f *contracts.FileFacts) { f.ConfigKeys[0].ID = "" }},
		{"config key with proxy ID", "nonportable parse node", func(f *contracts.FileFacts) { f.ConfigKeys[0].ID = "remote:repo~cfg" }},
		{"config key with absolute path", "nonportable parse node", func(f *contracts.FileFacts) { f.ConfigKeys[0].AbsoluteFilePath = "/app/config.yml" }},
		{"config key with repository scope", "nonportable parse node", func(f *contracts.FileFacts) { f.ConfigKeys[0].RepoPrefix = "repo" }},
		{"config key with workspace scope", "nonportable parse node", func(f *contracts.FileFacts) { f.ConfigKeys[0].WorkspaceID = "work" }},
		{"config key with project scope", "nonportable parse node", func(f *contracts.FileFacts) { f.ConfigKeys[0].ProjectID = "project" }},
		{"config key with wrong kind", "contract config key identity or redaction", func(f *contracts.FileFacts) {
			f.ConfigKeys[0].Kind = graph.KindFunction
		}},
		{"config key outside file", "contract config key identity or redaction", func(f *contracts.FileFacts) {
			f.ConfigKeys[0].FilePath = "other.yml"
		}},
		{"config key without redaction flag", "contract config key identity or redaction", func(f *contracts.FileFacts) {
			delete(f.ConfigKeys[0].Meta, "value_redacted")
		}},
		{"config key with false redaction flag", "contract config key identity or redaction", func(f *contracts.FileFacts) {
			f.ConfigKeys[0].Meta["value_redacted"] = false
		}},
		{"config key with non-boolean redaction flag", "contract config key identity or redaction", func(f *contracts.FileFacts) {
			f.ConfigKeys[0].Meta["value_redacted"] = "true"
		}},
		{"config key with non-string source", "contract config key metadata", func(f *contracts.FileFacts) {
			f.ConfigKeys[0].Meta["source"] = 1
		}},
		{"config key with non-string raw key", "contract config key metadata", func(f *contracts.FileFacts) {
			f.ConfigKeys[0].Meta["raw_key"] = 1
		}},
		{"config key with non-string profile", "contract config key metadata", func(f *contracts.FileFacts) {
			f.ConfigKeys[0].Meta["profile"] = 1
		}},
		{"config key with unexpected metadata", "contract config key metadata", func(f *contracts.FileFacts) {
			f.ConfigKeys[0].Meta["value"] = "plaintext"
		}},
		{"missing router for Go route question", "", func(f *contracts.FileFacts) { f.Router = nil }},
		{"negative router line count", "contract router line count", func(f *contracts.FileFacts) {
			f.Router.Lines = nil
			f.Router.LineCount = -1
		}},
		{"router line order goes backwards", "contract router line order", func(f *contracts.FileFacts) {
			f.Router.Lines[0], f.Router.Lines[1] = f.Router.Lines[1], f.Router.Lines[0]
		}},
		{"duplicate router line", "contract router line order", func(f *contracts.FileFacts) { f.Router.Lines[1].Line = 1 }},
		{"router line beyond line count", "contract router line order", func(f *contracts.FileFacts) {
			f.Router.LineCount = 1
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			facts := newFacts()
			tt.invalidate(facts)
			requireContractFactsRefusal(t, base, facts, tt.reason)
		})
	}
}

func requireContractFactsRefusal(t *testing.T, base ParseFacts, facts *contracts.FileFacts, reason string) {
	t.Helper()
	_, err := EncodeContractParseFacts(base, facts)
	require.ErrorIs(t, err, errInvalidRecord, "encode")
	if reason != "" {
		require.ErrorContains(t, err, reason, "encode")
	}

	// Bypass only semantic checks, so the real decoder must refuse the receipt.
	v2, err := EncodeParseFacts(base)
	require.NoError(t, err)
	wire, err := encodeContractFactSuffix(v2, facts)
	require.NoError(t, err)
	_, _, _, err = DecodeContractParseFacts(wire)
	require.ErrorIs(t, err, errInvalidRecord, "decode")
	if reason != "" {
		require.ErrorContains(t, err, reason, "decode")
	}
}

func TestContractParseFactsHandlerTreeAnswers(t *testing.T) {
	for _, tt := range []struct {
		name, path, language string
		hasAST, hasTree      bool
	}{
		{"Go tree with empty bindings", "file.go", "go", true, true},
		{"Go without tree", "file.go", "go", false, false},
		{"Go extractor on a non-Go path", "file.txt", "go", true, false},
		{"non-Go extractor on a Go path", "file.go", "php", true, true},
		{"PHP without Go tree", "file.php", "php", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := tt.path + "::H"
			base := ParseFacts{
				File:              graph.FileMetaRow{FilePath: tt.path, NodeCount: 1},
				EffectiveLanguage: tt.language,
				Nodes:             []*graph.Node{{ID: id, Kind: graph.KindFunction, FilePath: tt.path}},
			}
			h := contracts.HandlerFacts{Regex: map[string]any{"status_codes": []int{201}}}
			if tt.hasAST {
				h.AST = map[string]any{}
			}
			if tt.hasTree {
				h.Bindings = map[string]contracts.Binding{}
			}
			facts := &contracts.FileFacts{
				Shapes: map[string]*contracts.Shape{}, Handlers: map[string]contracts.HandlerFacts{id: h},
			}
			wire, err := EncodeContractParseFacts(base, facts)
			require.NoError(t, err)
			_, got, _, err := DecodeContractParseFacts(wire)
			require.NoError(t, err)
			require.Equal(t, facts, got)
			if tt.hasTree {
				h.Bindings = nil
				facts.Handlers[id] = h
				requireContractFactsRefusal(t, base, facts, "incomplete Go contract handler tree answer")
			}
		})
	}
}

func TestContractParseFactsPHPClassAnswerScope(t *testing.T) {
	const path = "routes.php"
	base := ParseFacts{File: graph.FileMetaRow{FilePath: path}, EffectiveLanguage: "php"}
	facts := &contracts.FileFacts{
		Shapes: map[string]*contracts.Shape{}, Handlers: map[string]contracts.HandlerFacts{},
		Records: []contracts.Contract{{
			ID:       "http::GET::/orders",
			FilePath: path,
			Type:     contracts.ContractHTTP,
			Role:     contracts.RoleProvider,
			Line:     4,
			Meta:     map[string]any{"framework": "custom", "handler_trail": "Orders.show", "handler_class": "Orders"},
		}},
	}
	const reason = "missing or invalid Laravel handler class answer"
	requireContractFactsRefusal(t, base, facts, reason)
	facts.LaravelHandlerClasses = map[string]string{"4:Orders": "Acme\\Orders"}
	wire, err := EncodeContractParseFacts(base, facts)
	require.NoError(t, err)
	_, got, _, err := DecodeContractParseFacts(wire)
	require.NoError(t, err)
	require.Equal(t, facts, got)

	t.Run("PHP path with non-PHP effective language", func(t *testing.T) {
		refined := base
		refined.EffectiveLanguage = "go"
		wire, err := EncodeContractParseFacts(refined, facts)
		require.NoError(t, err)
		_, got, _, err := DecodeContractParseFacts(wire)
		require.NoError(t, err)
		require.Equal(t, facts, got)
		missing := *facts
		missing.LaravelHandlerClasses = nil
		requireContractFactsRefusal(t, refined, &missing, reason)
	})
	for _, tt := range []struct {
		name   string
		change func(*contracts.Contract)
	}{
		{"schema extracted", func(c *contracts.Contract) { c.Meta["schema_source"] = "extracted" }},
		{"schema partial", func(c *contracts.Contract) { c.Meta["schema_source"] = "partial" }},
		{"no trail or identifier", func(c *contracts.Contract) { delete(c.Meta, "handler_trail") }},
		{"no class", func(c *contracts.Contract) { delete(c.Meta, "handler_class") }},
		{"consumer", func(c *contracts.Contract) { c.Role = contracts.RoleConsumer }},
		{"not HTTP", func(c *contracts.Contract) { c.Type = contracts.ContractGRPC }},
		{"not a PHP path", func(c *contracts.Contract) { c.FilePath = "routes.txt" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			record := facts.Records[0]
			record.Meta = map[string]any{"framework": "custom", "handler_trail": "Orders.show", "handler_class": "Orders"}
			tt.change(&record)
			base := base
			base.File.FilePath = record.FilePath
			withoutClass := *facts
			withoutClass.Records = []contracts.Contract{record}
			withoutClass.LaravelHandlerClasses = nil
			wire, err := EncodeContractParseFacts(base, &withoutClass)
			require.NoError(t, err)
			_, got, _, err := DecodeContractParseFacts(wire)
			require.NoError(t, err)
			require.Equal(t, &withoutClass, got)

			withoutClass.LaravelHandlerClasses = facts.LaravelHandlerClasses
			requireContractFactsRefusal(t, base, &withoutClass, "unattributed Laravel handler class answers")
		})
	}
}

func TestContractParseFactsRequiresRouter(t *testing.T) {
	for _, tt := range []struct {
		name, path, language, framework string
	}{
		{"census by extension", "mount.py", "text", ""},
		{"census by effective language", "mount.txt", "python", ""},
		{"supported HTTP framework", "routes.go", "go", "gin/echo/chi"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := ParseFacts{File: graph.FileMetaRow{FilePath: tt.path}, EffectiveLanguage: tt.language}
			facts := &contracts.FileFacts{Shapes: map[string]*contracts.Shape{}, Handlers: map[string]contracts.HandlerFacts{}}
			if tt.framework != "" {
				facts.Records = []contracts.Contract{{
					ID:       "http::GET::/items",
					FilePath: tt.path,
					Type:     contracts.ContractHTTP,
					Role:     contracts.RoleProvider,
					Meta:     map[string]any{"framework": tt.framework},
				}}
			}
			requireContractFactsRefusal(t, base, facts, "")
			facts.Router = &contracts.RouterFacts{}
			wire, err := EncodeContractParseFacts(base, facts)
			require.NoError(t, err)
			_, got, _, err := DecodeContractParseFacts(wire)
			require.NoError(t, err)
			require.Equal(t, facts, got)
		})
	}
}

func TestContractParseFactsRoundTripsNativeMetadata(t *testing.T) {
	const path = "file.go"
	base := ParseFacts{File: graph.FileMetaRow{FilePath: path}, EffectiveLanguage: "go"}
	values := map[string]any{
		"int64s":  []int64{math.MinInt64, 1 << 40},
		"uint64s": []uint64{0, math.MaxUint64},
		"bools":   []bool{true, false, true},
		"intMap":  map[string]int{"a": -3, "b": 2},
		"strMap":  map[string]string{"b": "\xff", "a": ""},
		"shape": &contracts.Shape{
			Kind:   "struct",
			Fields: []contracts.ShapeField{{Name: "Count", Type: "int", JSONTag: "count", Required: true}},
			Notes:  []string{},
		},
	}
	wantBits := []uint64{0x0000000000000001, 0x8000000000000000, 0x7ff8000000000042, 0xfff0000000000000}
	floats := make([]float64, 0, len(wantBits))
	for _, bits := range wantBits {
		floats = append(floats, math.Float64frombits(bits))
	}
	facts := &contracts.FileFacts{
		Shapes: map[string]*contracts.Shape{}, Handlers: map[string]contracts.HandlerFacts{},
		Records: []contracts.Contract{{
			ID:       "http::GET::/items",
			FilePath: path,
			Type:     contracts.ContractHTTP,
			Role:     contracts.RoleProvider,
			Meta:     map[string]any{"values": values, "float": floats[2], "floats": floats},
		}},
	}
	wire, err := EncodeContractParseFacts(base, facts)
	require.NoError(t, err)
	_, got, _, err := DecodeContractParseFacts(wire)
	require.NoError(t, err)
	require.Len(t, got.Records, 1)
	meta := got.Records[0].Meta
	require.Len(t, meta, 3)
	require.Equal(t, values, meta["values"])
	require.IsType(t, float64(0), meta["float"])
	require.Equal(t, wantBits[2], math.Float64bits(meta["float"].(float64)))
	require.IsType(t, []float64{}, meta["floats"])
	gotFloats := meta["floats"].([]float64)
	require.Len(t, gotFloats, len(wantBits))
	for i, want := range wantBits {
		require.Equal(t, want, math.Float64bits(gotFloats[i]), "float at index %d", i)
	}
}

func TestContractParseFactsRejectsMalformedCollections(t *testing.T) {
	const path = "file.go"
	base := ParseFacts{File: graph.FileMetaRow{FilePath: path}, EffectiveLanguage: "go"}
	facts := &contracts.FileFacts{Shapes: map[string]*contracts.Shape{}, Handlers: map[string]contracts.HandlerFacts{}}
	wire, err := EncodeContractParseFacts(base, facts)
	require.NoError(t, err)
	d := recordCursor{buf: wire[1:]}
	_ = d.bytes() // Skip the v2 base.
	require.False(t, d.boolean())
	require.NoError(t, d.err)
	require.NotEmpty(t, d.buf)
	require.Equal(t, binary.AppendVarint(nil, -1)[0], d.buf[0]) // Nil Records.
	d.buf[0] = binary.AppendVarint(nil, -2)[0]
	_, _, _, err = DecodeContractParseFacts(wire)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "contract fact count")

	const firstKey, secondKey = "wire-key-a", "wire-key-b"
	facts.Records = []contracts.Contract{{
		ID:       "http::GET::/items",
		FilePath: path,
		Type:     contracts.ContractHTTP,
		Role:     contracts.RoleProvider,
		Meta:     map[string]any{firstKey: 1, secondKey: 2},
	}}
	wire, err = EncodeContractParseFacts(base, facts)
	require.NoError(t, err)
	_, _, _, err = DecodeContractParseFacts(wire)
	require.NoError(t, err)
	for _, tt := range []struct {
		name, key, reason string
		replace           []byte
	}{
		{"unsorted keys", firstKey, "contract fact map order", appendMetaString(nil, "wire-key-c")},
		{"duplicate keys", firstKey, "contract fact map order", appendMetaString(nil, secondKey)},
		// This invalid length would return an empty key, which also sorts before the first key.
		{"malformed second key", secondKey, "truncated or overflowing length", []byte{0xff}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bad := bytes.Clone(wire)
			key := appendMetaString(nil, tt.key)
			require.Equal(t, 1, bytes.Count(bad, key))
			copy(bad[bytes.Index(bad, key):], tt.replace)
			_, _, _, err := DecodeContractParseFacts(bad)
			require.ErrorIs(t, err, errInvalidRecord)
			require.ErrorContains(t, err, tt.reason)
		})
	}
}

func TestContractParseFactsLimitsMetadataNesting(t *testing.T) {
	const path = "file.go"
	const leaf = "contract-nesting-leaf"
	base := ParseFacts{File: graph.FileMetaRow{FilePath: path}, EffectiveLanguage: "go"}
	var nested any = leaf
	for range metaMaxNativeDepth - 1 {
		nested = []any{nested}
	}
	facts := &contracts.FileFacts{
		Shapes: map[string]*contracts.Shape{}, Handlers: map[string]contracts.HandlerFacts{},
		Records: []contracts.Contract{{
			ID:       "http::GET::/items",
			FilePath: path,
			Type:     contracts.ContractHTTP,
			Role:     contracts.RoleProvider,
			Meta:     map[string]any{"nested": nested},
		}},
	}
	wire, err := EncodeContractParseFacts(base, facts)
	require.NoError(t, err)
	_, got, _, err := DecodeContractParseFacts(wire)
	require.NoError(t, err)
	require.Equal(t, facts, got)

	facts.Records[0].Meta["nested"] = []any{nested}
	_, err = EncodeContractParseFacts(base, facts)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "contract metadata nesting")

	// Insert one singleton slice around the leaf without asking the encoder
	// to produce a value beyond its nesting limit.
	encodedLeaf := appendMetaString([]byte{metaTagString}, leaf)
	require.Equal(t, 1, bytes.Count(wire, encodedLeaf))
	pos := bytes.Index(wire, encodedLeaf)
	bad := binary.AppendVarint(append(bytes.Clone(wire[:pos]), metaTagAnySlice), 1)
	bad = append(bad, wire[pos:]...)
	_, _, _, err = DecodeContractParseFacts(bad)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "truncated or deeply nested contract metadata")
}

func TestContractMetadataReadersKeepFirstError(t *testing.T) {
	firstErr := invalid("earlier cursor error")
	// This buffer is invalid as a varint but readable as a float. None of
	// the three readers should read it after a previous failure.
	raw := bytes.Repeat([]byte{0xff}, 8)
	for _, tt := range []struct {
		name string
		read func(*recordCursor)
	}{
		{"int64", func(d *recordCursor) { _ = readFactInt64(d) }},
		{"uint64", func(d *recordCursor) { _ = readFactUint64(d) }},
		{"float64", func(d *recordCursor) { _ = readFactFloat(d) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := recordCursor{buf: bytes.Clone(raw), err: firstErr}
			tt.read(&d)
			require.ErrorIs(t, d.err, firstErr)
			require.Equal(t, raw, d.buf)
		})
	}
}
