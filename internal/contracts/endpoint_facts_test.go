package contracts

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
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
	nodes := []*graph.Node{{ID: path+"::Register", Name: "Register", Kind: graph.KindFunction, FilePath: path, StartLine: 2, EndLine: 8}, {ID: path+"::H", Name: "H", Kind: graph.KindFunction, FilePath: path, StartLine: 9, EndLine: 9}}
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
	for _, tc := range []struct{
		name string
		ex StoreAwareExtractor
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
			got := ResolveEndpointQuestions(raw, capture.Questions, g)
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
		{ID: path+"::A.Handle", Name: "Handle", Kind: graph.KindMethod, FilePath: path, StartLine: 2, EndLine: 2},
		{ID: path+"::B.Handle", Name: "Handle", Kind: graph.KindMethod, FilePath: path, StartLine: 2, EndLine: 2},
		{ID: path+"::Other", Name: "Other", Kind: graph.KindFunction, FilePath: path, StartLine: 3, EndLine: 3},
	}
	lines := []string{"package p", "func (A) Handle(w Writer) { WriteJSON(w, 200, First{}) }; func (B) Handle(w Writer) { WriteJSON(w, 201, Second{}) }", "func Other(w Writer) { value, err := svc.Get(); _ = err; WriteJSON(w, 200, value) }", ""}
	facts, err := CaptureFileHandlerFacts(ctx, nodes, lines, "go", tree)
	require.NoError(t, err)
	require.Len(t, facts, len(nodes))
	for _, n := range nodes {
		for _, seeded := range []bool{false, true} {
			makeContract := func() Contract {
				c := Contract{Role: RoleProvider, SymbolID: n.ID, FilePath: path, Line: n.StartLine, Meta: map[string]any{"path":"/items/{p1}", "status_codes": []int{404}, "response_expr": "old"}}
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
		name string
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
		got := ResolveEndpointQuestions(raw, capture.Questions, g)
		require.Equal(t, want, got)
	}
}
