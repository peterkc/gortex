package languages

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestJSTS_BuiltinReceiverFacts(t *testing.T) {
	for _, tt := range []struct {
		name      string
		path      string
		extractor parser.Extractor
		src       string
		want      map[int]graph.BuiltinReceiverHint
	}{
		{
			name: "javascript", path: "example.js", extractor: NewJavaScriptExtractor(),
			src: `import defaultImport, { Array as ImportedArray } from "custom-lib";
import * as ns from "ns-lib";
[1].slice(0);
"ab".slice(0);
(new Map()).get("x");
Array.from([1]);
ImportedArray.from([]);
defaultImport.slice();
ns.value.slice();
function run(console) { console.log("x"); }
function check(untyped) { untyped.slice(0); }
function ordinary() {}
ordinary();
const api = { slice() {} };
api.slice();
(new LocalWidget()).slice();`,
			want: map[int]graph.BuiltinReceiverHint{
				3:  {Form: "instance", Family: "array"},
				4:  {Form: "instance", Family: "string"},
				5:  {Form: "instance", Family: "map"},
				6:  {Form: "static", Family: "array"},
				7:  {Form: "import", Name: "Array", Module: "custom-lib"},
				8:  {Form: "import", Name: "default", Module: "custom-lib"},
				9:  {Form: "import", Name: "*", Module: "ns-lib"},
				10: {Form: "shadowed"},
				11: {Form: "unknown"},
				16: {Form: "user", Name: "LocalWidget"},
			},
		},
		{
			name: "typescript", path: "example.tsx", extractor: NewTypeScriptExtractor(),
			src: `import tDefault, { Box as ImportedBox } from "custom-lib";
import * as ns from "ns-lib";
function run(items: Item[], text: string, box: LocalBox) {
  items.slice(0);
  text.slice(0);
  box.slice();
}
let local: Item[];
local.slice(0);
let mixed: string = [];
mixed.slice(0);
tDefault.slice(0);
ns.slice(0);
ImportedBox.slice(0);
const view = <div />;`,
			want: map[int]graph.BuiltinReceiverHint{
				4:  {Form: "instance", Family: "array"},
				5:  {Form: "instance", Family: "string"},
				6:  {Form: "user", Name: "LocalBox"},
				9:  {Form: "instance", Family: "array"},
				11: {Form: "shadowed"},
				12: {Form: "import", Name: "default", Module: "custom-lib"},
				13: {Form: "import", Name: "*", Module: "ns-lib"},
				14: {Form: "import", Name: "Box", Module: "custom-lib"},
			},
		},
		{
			name: "unsupported_binding_or_write", path: "ambiguous.js", extractor: NewJavaScriptExtractor(),
			src: `const xs = [1];
[xs] = ["wrong"];
xs.slice(0);
function f({ Map }) {
  (new Map()).get("x");
}
for (let Array of custom) { Array.from([]); }
const { JSON } = custom;
JSON.stringify("x");
const clean = [1];
clean.slice = custom;
clean.slice(0);`,
			want: map[int]graph.BuiltinReceiverHint{
				3:  {Form: "shadowed"},
				5:  {Form: "shadowed"},
				7:  {Form: "shadowed"},
				9:  {Form: "shadowed"},
				12: {Form: "shadowed"},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := []byte(tt.src)
			native, err := parser.Extract(tt.extractor, tt.path, src, parser.ExtractionOptions{})
			require.NoError(t, err)
			captured, err := parser.Extract(tt.extractor, tt.path, src, parser.ExtractionOptions{CaptureBuiltinReceiverFacts: true})
			require.NoError(t, err)
			require.ElementsMatch(t, native.Nodes, captured.Nodes)

			seen := make(map[int]int)
			want := make([]*graph.Edge, 0, len(native.Edges))
			for _, edge := range native.Edges {
				copy := *edge
				if hint, ok := tt.want[edge.Line]; ok && edge.Kind == graph.EdgeCalls {
					seen[edge.Line]++
					hint.Language = tt.extractor.Language()
					_, hasHint := edge.BuiltinReceiverHint()
					require.False(t, hasHint, "native line %d", edge.Line)
					copy.Meta = maps.Clone(edge.Meta)
					copy.SetBuiltinReceiverHint(hint)
					got, ok := copy.BuiltinReceiverHint()
					require.True(t, ok)
					require.Equal(t, hint, got)
				}
				want = append(want, &copy)
			}
			for line := range tt.want {
				require.Equal(t, 1, seen[line], "Call at line %d", line)
			}
			require.ElementsMatch(t, want, captured.Edges)
		})
	}
}
