package contracts

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/tsx"
	"github.com/zzet/gortex/internal/parser/tsitter/typescript"
)

func TestEndpointContractQuestionsNativeOrder(t *testing.T) {
	const path = "routes.go"
	src := []byte(`package p
func Register(r Router) {
	r.GET("/before", H)
	r.GET(Route, H)
	r.GET(Missing, H)
	r.GET("/after", H)
	client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: Queue})
}
func H() {}
`)
	tree := ParseTreeForLang("go", src)
	require.NotNil(t, tree)
	defer tree.Release()
	nodes := []*graph.Node{{ID: path + "::Register", Name: "Register", Kind: graph.KindFunction, FilePath: path, StartLine: 2, EndLine: 8}, {ID: path + "::H", Name: "H", Kind: graph.KindFunction, FilePath: path, StartLine: 9, EndLine: 9}}
	g := graph.New()
	consts := []graph.ConstantValueRow{
		{NodeID: "const.go::Route", FilePath: "const.go", Value: "/api/{id}"},
		{NodeID: "const.go::Queue", FilePath: "const.go", Value: "orders"},
	}
	for _, c := range consts {
		name := "Route"
		if c.NodeID == "const.go::Queue" {
			name = "Queue"
		}
		g.AddNode(&graph.Node{ID: c.NodeID, Name: name, Kind: graph.KindConstant, FilePath: c.FilePath})
	}
	require.NoError(t, g.BulkSetConstantValues("", consts))
	for _, tc := range []struct {
		name      string
		ex        StoreAwareExtractor
		questions int
	}{
		{"http", &HTTPExtractor{}, 2},
		{"topic", &TopicExtractor{}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &EndpointFactCapture{}
			raw := tc.ex.ExtractWithStore(path, src, nodes, nil, tree, capture, "")
			require.Len(t, capture.Questions, tc.questions)
			want := tc.ex.ExtractWithStore(path, src, nodes, nil, tree, g, "")
			got, err := ResolveEndpointQuestions(raw, capture.Questions, g)
			require.NoError(t, err)
			require.Equal(t, want, got)
			if tc.name == "http" {
				require.Len(t, got, 3)
				require.Equal(t, "http::GET::/before", got[0].ID)
				require.Equal(t, "http::GET::/api/{p1}", got[1].ID)
				require.Equal(t, []string{"id"}, got[1].Meta["path_param_names"])
				require.Equal(t, "http::GET::/after", got[2].ID)
			}
		})
	}
}

// Native lazy body selection is first row, then name fallback. In particular
// same-name declarations on the same line must not substitute different ASTs.
func TestFileHandlerFactsNativeMetadata(t *testing.T) {
	ctx := t.Context()
	const path = "handler.go"
	src := []byte(`package p
func (A) Handle(w Writer) { WriteJSON(w, 200, First{}) }; func (B) Handle(w Writer) { WriteJSON(w, 201, Second{}) }
func Other(w Writer) { value, err := svc.Get(); _ = err; WriteJSON(w, 200, value) }
`)
	tree := ParseTreeForLang("go", src)
	require.NotNil(t, tree)
	defer tree.Release()
	nodes := []*graph.Node{
		{ID: path + "::A.Handle", Name: "Handle", Kind: graph.KindMethod, FilePath: path, StartLine: 2, EndLine: 2},
		{ID: path + "::B.Handle", Name: "Handle", Kind: graph.KindMethod, FilePath: path, StartLine: 2, EndLine: 2},
		{ID: path + "::Other", Name: "Other", Kind: graph.KindFunction, FilePath: path, StartLine: 3, EndLine: 3},
	}
	lines := []string{"package p", "func (A) Handle(w Writer) { WriteJSON(w, 200, First{}) }; func (B) Handle(w Writer) { WriteJSON(w, 201, Second{}) }", "func Other(w Writer) { value, err := svc.Get(); _ = err; WriteJSON(w, 200, value) }", ""}
	facts, err := CaptureFileHandlerFacts(ctx, nodes, lines, "go", tree)
	require.NoError(t, err)
	require.Len(t, facts, len(nodes))
	for _, n := range nodes {
		for _, seeded := range []bool{false, true} {
			makeContract := func() Contract {
				c := Contract{Role: RoleProvider, SymbolID: n.ID, FilePath: path, Line: n.StartLine, Meta: map[string]any{"path": "/items/{p1}", "status_codes": []int{404}, "response_expr": "old"}}
				if seeded {
					c.Meta["response_type"] = "Seed"
				}
				return c
			}
			want, got := makeContract(), makeContract()
			EnrichHTTPContractWithTree(&want, lines, nodes, "go", tree)
			facts[n.ID].EnrichProvider(&got)
			require.Equal(t, want.Meta, got.Meta, n.ID)
		}
	}
	require.Equal(t, MakeBodyFacts(tree, nodes[2]).VarBinding("value"), facts[nodes[2].ID].VarBinding("value"))
}

func TestEndpointClientQuestionsNativeDedup(t *testing.T) {
	src := []byte(`import requests

def fetch():
    return requests.get(URL), requests.get("/api/items")
`)
	tree := buildClientLibTree("python", src)
	require.NotNil(t, tree)
	defer tree.Release()
	const path = "client.py"
	nodes := makeNodes(path, []struct {
		name       string
		start, end int
	}{{"fetch", 3, 4}})
	ex := &HTTPExtractor{}
	capture := &EndpointFactCapture{}
	raw := ex.ExtractWithStore(path, src, nodes, nil, tree, capture, "")
	require.NotEmpty(t, capture.Questions)
	g := graph.New()
	g.AddNode(&graph.Node{ID: "const.py::URL", Name: "URL", Kind: graph.KindConstant, FilePath: "const.py"})
	for _, value := range []string{"/api/items", "/api/new"} {
		require.NoError(t, g.BulkSetConstantValues("", []graph.ConstantValueRow{{NodeID: "const.py::URL", FilePath: "const.py", Value: value}}))
		want := ex.ExtractWithStore(path, src, nodes, nil, tree, g, "")
		got, err := ResolveEndpointQuestions(raw, capture.Questions, g)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

func TestGRPCContractFactTreeChecksNativeGrammar(t *testing.T) {
	src := []byte("import { UsersClient } from './users_pb'; const client = new UsersClient('host');")
	for _, tc := range []struct {
		path    string
		grammar *sitter.Language
		want    bool
	}{
		{"client.ts", typescript.GetLanguage(), true},
		{"client.ts", tsx.GetLanguage(), false},
		{"client.tsx", tsx.GetLanguage(), true},
		{"client.tsx", typescript.GetLanguage(), false},
		{"client.jsx", tsx.GetLanguage(), true},
		{"client.jsx", typescript.GetLanguage(), false},
		{"client.TSX", typescript.GetLanguage(), false},
		{"client.TSX", tsx.GetLanguage(), true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			raw, err := parser.ParseFile(src, tc.grammar)
			require.NoError(t, err)
			require.NotNil(t, raw)
			tree := parser.NewParseTree(raw, src, "typescript")
			t.Cleanup(tree.Release)
			err = CheckFileFactTree(tc.path, src, tree, []Extractor{&GRPCExtractor{}})
			if !tc.want {
				require.ErrorIs(t, err, ErrContractFactTree)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tsFileImportsGRPC(tc.path, src, nil), tsFileImportsGRPC(tc.path, src, tree))
			require.True(t, tsFileImportsGRPC(tc.path, src, tree))
		})
	}
}

func TestContractFactTreeNativeGuards(t *testing.T) {
	src := []byte("package p\nfunc f() {}\n")
	tree := ParseTreeForLang("go", src)
	require.NotNil(t, tree)
	defer tree.Release()
	require.ErrorIs(t, CheckFileFactTree("x.go", []byte("x"), tree, nil), ErrContractFactTree)
	require.ErrorIs(t, CheckFileFactTree("x.go", src, nil, []Extractor{&TopicExtractor{}}), ErrContractFactTree)
	grpcText := []byte(`// grpc and new UsersClient("host")
package p
func f() {}`)
	for _, path := range []string{"x.go", "x.py", "x.proto"} {
		before := parser.ParseCount()
		require.NoError(t, CheckFileFactTree(path, grpcText, nil, []Extractor{&GRPCExtractor{}}))
		(&GRPCExtractor{}).Extract(path, grpcText, nil, nil)
		require.Equal(t, before, parser.ParseCount(), path)
	}
	// Isolate the custom factory guard from record extraction, as well.
	original := FactoryForLang("go")
	t.Cleanup(func() { RegisterBodyFactsFactory("go", original) })
	RegisterBodyFactsFactory("go", func(*parser.ParseTree, *graph.Node) BodyFacts { return nopBodyFacts{} })
	// No detector requires a tree here: the refusal must come from the factory guard.
	require.ErrorIs(t, CheckFileFactTree("x.go", src, tree, nil), ErrContractFactTree)
	_, err := CaptureFileHandlerFacts(t.Context(), nil, nil, "go", tree)
	require.ErrorIs(t, err, ErrContractFactTree)
}

func TestGRPCContractFactNoAdditionalParse(t *testing.T) {
	const path = "client.ts"
	src := []byte("import { UsersClient } from './users_pb'; const client = new UsersClient('host');")
	raw, err := parser.ParseFile(src, typescript.GetLanguage())
	require.NoError(t, err)
	tree := parser.NewParseTree(raw, src, "typescript")
	defer tree.Release()
	before := parser.ParseCount()
	got := (&GRPCExtractor{}).ExtractWithTree(path, src, nil, nil, tree)
	require.NotEmpty(t, got)
	require.Equal(t, before, parser.ParseCount())
	want := (&GRPCExtractor{}).Extract(path, src, nil, nil)
	require.Equal(t, before+1, parser.ParseCount())
	require.Equal(t, want, got)
}

func TestEndpointClientLiteralCapture(t *testing.T) {
	const path = "client.py"
	src := []byte("import requests\ndef fetch():\n    return requests.get('/api/items')\n")
	tree := buildClientLibTree("python", src)
	require.NotNil(t, tree)
	defer tree.Release()
	ex := &HTTPExtractor{}
	capture := &EndpointFactCapture{}
	got := ex.ExtractWithStore(path, src, nil, nil, tree, capture, "")
	require.Empty(t, capture.Questions)
	require.NotEmpty(t, got)
	require.Equal(t, ex.ExtractWithStore(path, src, nil, nil, tree, nil, ""), got)
}

func TestEndpointQuestionPositionsChecked(t *testing.T) {
	raw := []Contract{{ID: "one"}}
	for _, positions := range [][]int{{-1}, {2}, {1, 0}} {
		questions := make([]EndpointContractQuestion, len(positions))
		for i, pos := range positions {
			questions[i].Position = pos
		}
		got, err := ResolveEndpointQuestions(raw, questions, nil)
		require.ErrorIs(t, err, ErrEndpointQuestionPosition)
		require.Nil(t, got)
	}
}

func TestEndpointGoQuestionSkipsStdlibConsumer(t *testing.T) {
	src := []byte(`package p
func f() { http.Get(Route); router.GET(Route, f) }`)
	tree := ParseTreeForLang("go", src)
	require.NotNil(t, tree)
	defer tree.Release()
	capture := &EndpointFactCapture{}
	(&HTTPExtractor{}).ExtractWithStore("x.go", src, nil, nil, tree, capture, "")
	require.Len(t, capture.Questions, 1)
	require.Equal(t, "GET", capture.Questions[0].Method)
}
