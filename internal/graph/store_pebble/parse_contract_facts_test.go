package store_pebble

import (
	"bytes"
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
		RepoPrefix: "repo", WorkspaceID: "work", ProjectID: "project",
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
		Position: 1, Kind: contracts.EndpointGoRoute,
		Expression: contracts.EndpointExpression{Terms: []contracts.EndpointTerm{{Name: "VALUE"}}},
		Record:     contracts.Contract{FilePath: "empty.go", Type: contracts.ContractHTTP, Role: contracts.RoleProvider},
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
			ID: "http::GET::/orders", FilePath: path, Type: contracts.ContractHTTP, Role: contracts.RoleProvider, Line: 4,
			Meta: map[string]any{"framework": "laravel", "handler_class": "Orders"},
		}},
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
