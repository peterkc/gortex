package languages

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

func TestJSTS_BuiltinReceiverEvidence(t *testing.T) {
	for _, tc := range []struct{ name, lang, src, form, family string }{
		{"return", "javascript", `function f() { const items = []; items.push(1); return items; }`, "instance", "array"},
		{"argument", "javascript", `const xs = []; f(xs); xs.push(1);`, "instance", "array"},
		{"property_read", "javascript", `const xs = []; if (xs.length > 0) xs.push(1);`, "instance", "array"},
		{"iteration_source", "javascript", `const xs = []; for (const x of xs) {} xs.push(1);`, "instance", "array"},
		{"instanceof", "javascript", `x instanceof Array; Array.from(xs);`, "static", "array"},
		{"member_alias_read", "javascript", `const log = console.log; console.log(x);`, "static", "console"},
		{"call_initializer", "javascript", `const xs = getItems(); xs.push(1);`, "unknown", ""},
		{"member_alias_value", "javascript", `const log = console.log; log.slice();`, "unknown", ""},
		{"any", "typescript", `let xs: any; xs.slice();`, "unknown", ""},
		{"union", "typescript", `let xs: string | number[]; xs.slice();`, "unknown", ""},
		{"readonly_array", "typescript", `let xs: readonly T[]; xs.slice();`, "unknown", ""},
		{"promise_type", "typescript", `let xs: Promise<number>; xs.slice();`, "unknown", ""},
		{"array_generic", "typescript", `let xs: Array<string>; xs.slice();`, "unknown", ""},
		{"map_generic", "typescript", `let xs: Map<string, number>; xs.slice();`, "unknown", ""},
		{"typed_array", "typescript", `let xs: Uint8Array; xs.slice();`, "unknown", ""},
		{"weak_map_constructor", "javascript", `new WeakMap().get(key);`, "unknown", ""},
		{"url_member", "javascript", `new URL(u).searchParams.get("q");`, "unknown", ""},
		{"unknown_initializer_typed", "typescript", `const xs: number[] = getItems(); xs.slice();`, "unknown", ""},
		{"prototype_write_after_call", "javascript", `[].slice(); Array.prototype.slice = f;`, "shadowed", ""},
		{"global_write", "javascript", `globalThis.Array = X; [].slice();`, "shadowed", ""},
		{"prototype_other_member", "javascript", `Array.prototype.other = f; [].slice();`, "instance", "array"},
		{"global_other_family", "javascript", `globalThis.Array = X; "ab".slice();`, "instance", "string"},
		{"local_prototype", "javascript", `function f(Array) { Array.prototype.slice = g; } [].slice();`, "instance", "array"},
		{"prototype_read", "javascript", `const m = Array.prototype.slice; [].slice();`, "instance", "array"},
		{"update_target", "javascript", `let xs = []; xs++; xs.slice();`, "shadowed", ""},
		{"index_is_read", "javascript", `const key = []; dst[key] = value; key.slice();`, "instance", "array"},
		{"default_is_read", "javascript", `const xs = []; const {a = xs} = obj; xs.slice();`, "instance", "array"},
		{"computed_key_is_read", "javascript", `const xs = []; const {[xs]: x} = obj; xs.slice();`, "instance", "array"},
		{"arrow_parameter", "javascript", `const f = console => console.log(x);`, "shadowed", ""},
		{"catch_parameter", "javascript", `try {} catch (Array) { Array.from(xs); }`, "shadowed", ""},
		{"with", "javascript", `with (obj) { [].slice(); }`, "unknown", ""},
		{"eval", "javascript", `eval(code); [].slice();`, "unknown", ""},
		{"escape", "javascript", `const \u0041rray = X; [].slice();`, "unknown", ""},
		{"syntax_error", "javascript", `[].slice(); const broken = ;`, "unknown", ""},
		{"not_yet_visible", "javascript", `xs.slice(); const xs = [];`, "shadowed", ""},
		{"outside_block", "javascript", `{ const xs = []; } xs.slice();`, "shadowed", ""},
		{"bigint", "javascript", `(1n).toFixed(0);`, "user", ""},
		{"template", "javascript", "`hello ${x}`.slice();", "instance", "string"},
		{"export_const", "javascript", `export const xs = []; xs.slice();`, "instance", "array"},
		{"unknown_var_initializer", "javascript", `var xs = f(); xs.slice();`, "unknown", ""},
		{"named_local_constructor", "javascript", `class Box {} new Box().slice();`, "shadowed", ""},
		{"global_bracket_write", "javascript", `globalThis["Array"] = X; [].slice();`, "shadowed", ""},
		{"dynamic_prototype_member", "javascript", `Array.prototype[key] = f; [].slice();`, "unknown", ""},
		{"unrelated_local_member", "javascript", `const xs = []; xs.other = f; xs.slice();`, "instance", "array"},
		{"reassigned_global", "javascript", `Array = X; [].slice();`, "shadowed", ""},
		{"untyped_arrow", "javascript", `const fn = xs => xs.slice();`, "unknown", ""},
		{"optional_parameter", "typescript", `function f(xs?: any) { xs.slice(); }`, "unknown", ""},
		{"type_only_does_not_bind_value", "typescript", `type Array = {}; Array.from(xs);`, "static", "array"},
		{"instanceof_operand", "javascript", `function f(x) { x instanceof Array; x.slice(); }`, "unknown", ""},
		{"alias_annotation", "typescript", `type Items = string[]; let xs: Items; xs.slice();`, "unknown", ""},
		{"namespace_root", "typescript", `namespace Array.Custom {} Array.from(xs);`, "shadowed", ""},
		{"loop_value", "javascript", `for (const x of xs) { x.slice(); }`, "unknown", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.src)
			tree, err := parser.ParseFile(src, astquery.DefaultLanguageResolver(tc.lang))
			require.NoError(t, err)
			defer tree.Close()
			root := tree.RootNode()
			require.Equal(t, tc.name == "syntax_error", root.HasError())
			r := collectJSBuiltinReceivers(root, src, tc.lang)
			var hints []graph.BuiltinReceiverHint
			walkNodes(root, func(n *sitter.Node) {
				if n.Type() != "call_expression" {
					return
				}
				if fn := n.ChildByFieldName("function"); fn != nil && fn.Type() == "member_expression" {
					hints = append(hints, r.memberHint(fn, n.StartByte()))
				}
			})
			require.Equal(t, []graph.BuiltinReceiverHint{{Form: tc.form, Family: tc.family}}, hints)
		})
	}
}

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
				16: {Form: "unknown"},
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
const view = <div />;
interface LocalBox { slice(): void }
let imported: ImportedBox;
imported.slice();`,
			want: map[int]graph.BuiltinReceiverHint{
				4:  {Form: "instance", Family: "array"},
				5:  {Form: "instance", Family: "string"},
				6:  {Form: "user", Name: "LocalBox"},
				9:  {Form: "instance", Family: "array"},
				11: {Form: "shadowed"},
				12: {Form: "import", Name: "default", Module: "custom-lib"},
				13: {Form: "import", Name: "*", Module: "ns-lib"},
				14: {Form: "import", Name: "Box", Module: "custom-lib"},
				18: {Form: "import", Name: "Box", Module: "custom-lib"},
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
					if hint.Form == "unknown" {
						require.False(t, ok)
						require.Equal(t, edge.Meta, copy.Meta)
					} else {
						require.True(t, ok)
						require.Equal(t, hint, got)
					}
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
