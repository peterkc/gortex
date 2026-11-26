package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/ruby"
)

// qRubyAll is a single tree-sitter query alternating over every pattern
// the Ruby extractor needs. One tree walk per file replaces the 7
// `parser.RunQuery` calls the previous design made. Capture names are
// disjoint across patterns so the dispatch in Extract can branch on
// which name is set. Class/module membership for methods and singleton
// methods is resolved via a strict parent walk
// (method → body_statement → class) — nested defs inside another
// method's body fall through to the free-function bucket, mirroring
// the legacy nested-query semantics exactly.
const qRubyAll = `
[
  (class
    name: (constant) @class.name) @class.def

  (module
    name: (constant) @mod.name) @mod.def

  (method
    name: (identifier) @method.name) @method.def

  (singleton_method
    name: (identifier) @singleton.name) @singleton.def

  (call
    method: (identifier) @req.method
    arguments: (argument_list
      (string (string_content) @req.path))) @req.def

  (call
    method: (identifier) @call.name) @call.expr

  (body_statement
    (identifier) @bare.name) @bare.stmt

  (assignment
    left: (constant) @const.name
    right: (_) @const.value) @const.def
]
`

// rubyVisibilityMarkers are the Module visibility macros — recognised both as
// the section markers that flip subsequent method visibility and (in their
// bare form) excluded from the bare-call surface so they don't read as calls.
var rubyVisibilityMarkers = map[string]string{
	"private":              VisibilityPrivate,
	"protected":            VisibilityProtected,
	"public":               VisibilityPublic,
	"module_function":      VisibilityPrivate,
	"private_class_method": VisibilityPrivate,
}

// rubyMixinMacros are the module-composition macros: a class/module that calls
// one with a constant argument composes that module.
var rubyMixinMacros = map[string]bool{"include": true, "extend": true, "prepend": true}

// RubyExtractor extracts Ruby source files into graph nodes and edges.
type RubyExtractor struct {
	retainTree bool
	lang       *sitter.Language
	qAll       *parser.PreparedQuery
}

func NewRubyExtractor() *RubyExtractor {
	lang := ruby.GetLanguage()
	return &RubyExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qRubyAll, lang),
	}
}

func (e *RubyExtractor) Language() string     { return "ruby" }
func (e *RubyExtractor) Extensions() []string { return []string{".rb", ".rake", ".gemspec"} }

// --- Deferred call buffer ----------------------------------------

type rubyDeferredCall struct {
	name    string
	line    int
	hasRecv bool
	// receiver is the PascalCase constant receiver of a `Const.method` call
	// (`UserService`, `User`), or "" when the receiver is a variable / self /
	// literal. Stamped as Meta["recv_const"] so the Rails resolver can bind
	// the call to the directory-located service / model / helper definition.
	receiver string
	// returnUsage is how the call site consumes the return value
	// (graph.ReturnUsage* label), classified at capture time and
	// stamped as edge Meta on the EdgeCalls emitted for this site.
	returnUsage string
}

// WithRetainedTree requests the contract capture tree on an extraction-scoped copy.
func (e *RubyExtractor) WithRetainedTree() parser.Extractor {
	copy := *e
	copy.retainTree = true
	return &copy
}

func (e *RubyExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	keepTree := false
	defer func() {
		if !keepTree {
			tree.Close()
		}
	}()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID:        filePath,
		Kind:      graph.KindFile,
		Name:      filePath,
		FilePath:  filePath,
		StartLine: 1,
		EndLine:   int(root.EndPoint().Row) + 1,
		Language:  "ruby",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	var calls []rubyDeferredCall

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["class.def"] != nil:
			e.emitClass(m, filePath, fileID, src, result, seen)

		case m.Captures["mod.def"] != nil:
			e.emitModule(m, filePath, fileID, src, result, seen)

		case m.Captures["method.def"] != nil:
			e.emitMethod(m, filePath, fileID, src, result, seen)

		case m.Captures["singleton.def"] != nil:
			e.emitSingletonMethod(m, filePath, fileID, src, result, seen)

		case m.Captures["req.def"] != nil:
			e.emitRequire(m, filePath, fileID, result)

		case m.Captures["call.expr"] != nil:
			name := m.Captures["call.name"].Text
			if name == "require" || name == "require_relative" {
				// Handled by the require pattern above.
				return
			}
			expr := m.Captures["call.expr"]
			// Mixin composition: include/extend/prepend <Module> inside a
			// class/module body composes that module — emit a traversable
			// composition edge instead of a generic call to `include`.
			if rubyMixinMacros[name] {
				if typeID, ok := rubyEnclosingTypeID(expr.Node, src, filePath); ok {
					if e.emitRubyMixin(name, typeID, expr.Node, src, filePath, expr.StartLine+1, result) {
						return
					}
				}
			}
			hasRecv := false
			receiver := ""
			if expr.Node != nil {
				if recv := expr.Node.ChildByFieldName("receiver"); recv != nil {
					hasRecv = true
					receiver = rubyReceiverConst(recv, src)
				}
			}
			calls = append(calls, rubyDeferredCall{
				name:        name,
				line:        expr.StartLine + 1,
				hasRecv:     hasRecv,
				receiver:    receiver,
				returnUsage: classifyReturnUsage(expr.Node, src, rubyReturnUsageSpec),
			})

		case m.Captures["bare.stmt"] != nil:
			// A bare identifier in statement position is a paren-less,
			// receiver-less method call (a DSL macro / lifecycle hook like
			// `save`, `reload`, `validate`). Visibility markers are excluded —
			// they are handled by the visibility pass, not as calls.
			cap := m.Captures["bare.name"]
			bname := cap.Text
			if _, isVis := rubyVisibilityMarkers[bname]; isVis {
				return
			}
			calls = append(calls, rubyDeferredCall{name: bname, line: cap.StartLine + 1})

		case m.Captures["const.def"] != nil:
			e.emitConstant(m, filePath, fileID, result, seen)
		}
	})

	// Resolve call edges against funcRanges.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		target := "unresolved::" + c.name
		if c.hasRecv {
			target = "unresolved::*." + c.name
		}
		edge := &graph.Edge{
			From: callerID, To: target,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		}
		stampReturnUsage(edge, c.returnUsage)
		if c.receiver != "" {
			if edge.Meta == nil {
				edge.Meta = map[string]any{}
			}
			edge.Meta["recv_const"] = c.receiver
		}
		result.Edges = append(result.Edges, edge)
	}

	// Rails-style callback dispatch — preserves legacy behaviour exactly.
	emitRailsCallbacks(root, src, filePath, result)

	// Method visibility: walk each type body in order so a `private` /
	// `protected` section marker (and the targeted `private :foo` form) flips
	// the visibility of the methods it governs.
	applyRubyVisibility(root, src, filePath, result)

	captureValueRefCandidates(result, root, filePath, src)
	captureFnValueCandidates(result, root, filePath, src)
	captureSidekiqDispatch(result, root, filePath, src)
	if e.retainTree {
		result.ContractTree = parser.NewParseTree(tree, src, "ruby")
		keepTree = true
	}
	return result, nil
}

// rubyEnclosingTypeID walks up from node to the nearest enclosing class/module
// and returns its node ID, or ok=false when node is not inside a type body.
func rubyEnclosingTypeID(node *sitter.Node, src []byte, filePath string) (string, bool) {
	for n := node; n != nil; n = n.Parent() {
		if t := n.Type(); t == "class" || t == "module" {
			if name := n.ChildByFieldName("name"); name != nil {
				return filePath + "::" + name.Content(src), true
			}
			return "", false
		}
	}
	return "", false
}

// emitRubyMixin emits a composition edge (EdgeExtends, Meta via=include/extend/
// prepend) from typeID to each constant argument of an include/extend/prepend
// call, so the mixed-in module's members are reachable through the type — a
// dependency a plain call to `include` would hide. Returns true if it emitted.
func (e *RubyExtractor) emitRubyMixin(macro, typeID string, call *sitter.Node, src []byte, filePath string, line int, result *parser.ExtractionResult) bool {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return false
	}
	emitted := false
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		arg := args.NamedChild(i)
		if arg.Type() != "constant" && arg.Type() != "scope_resolution" {
			continue
		}
		mod := strings.TrimSpace(arg.Content(src))
		if mod == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: typeID, To: "unresolved::" + mod, Kind: graph.EdgeExtends,
			FilePath: filePath, Line: line, Meta: map[string]any{"via": macro},
		})
		emitted = true
	}
	return emitted
}

// applyRubyVisibility walks every class/module body in source order, tracking
// the active visibility section and the targeted `private :sym` forms, and
// stamps each method node's Meta["visibility"].
func applyRubyVisibility(root *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	methodByID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		if n != nil && n.Kind == graph.KindMethod {
			methodByID[n.ID] = n
		}
	}
	if len(methodByID) == 0 {
		return
	}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			c := n.NamedChild(i)
			if c.Type() == "class" || c.Type() == "module" {
				nameNode := c.ChildByFieldName("name")
				body := c.ChildByFieldName("body")
				if nameNode != nil && body != nil {
					applyRubyBodyVisibility(body, src, filePath, nameNode.Content(src), methodByID)
				}
			}
			walk(c)
		}
	}
	walk(root)
}

// applyRubyBodyVisibility applies the in-order visibility rules to one type body.
func applyRubyBodyVisibility(body *sitter.Node, src []byte, filePath, className string, methodByID map[string]*graph.Node) {
	current := VisibilityPublic
	setVis := func(method string, vis string) {
		if n := methodByID[filePath+"::"+className+"."+method]; n != nil {
			if n.Meta == nil {
				n.Meta = map[string]any{}
			}
			n.Meta["visibility"] = vis
		}
	}
	for i, _nc := 0, int(body.NamedChildCount()); i < _nc; i++ {
		c := body.NamedChild(i)
		switch c.Type() {
		case "identifier":
			if vis, ok := rubyVisibilityMarkers[c.Content(src)]; ok {
				current = vis
			}
		case "call":
			method := ""
			if mn := c.ChildByFieldName("method"); mn != nil {
				method = mn.Content(src)
			}
			vis, ok := rubyVisibilityMarkers[method]
			if !ok {
				continue
			}
			// Targeted form `private :a, :b` — flip only the named methods.
			if args := c.ChildByFieldName("arguments"); args != nil {
				for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
					for _, sym := range collectRubySymbols(args.NamedChild(i), src) {
						setVis(sym, vis)
					}
				}
			}
		case "method":
			if nm := c.ChildByFieldName("name"); nm != nil {
				setVis(nm.Content(src), current)
			}
		}
	}
}

// --- Per-match emit helpers -----------------------------------------

// rubyReceiverConst returns the constant name of a call receiver — `User` for
// `User.find`, the last segment of `Admin::User` for a scoped receiver — or ""
// when the receiver is not a constant (a variable, self, literal, ...).
func rubyReceiverConst(recv *sitter.Node, src []byte) string {
	if recv == nil {
		return ""
	}
	switch recv.Type() {
	case "constant":
		return recv.Content(src)
	case "scope_resolution":
		if name := recv.ChildByFieldName("name"); name != nil && name.Type() == "constant" {
			return name.Content(src)
		}
	}
	return ""
}

func (e *RubyExtractor) emitClass(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["class.name"].Text
	def := m.Captures["class.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": VisibilityPublic}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangHash); doc != "" {
		meta["doc"] = doc
	}
	meta["type_flavor"] = "class"
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "ruby",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	// ActiveRecord model attribution: emit EdgeModelsTable when the
	// class inherits from ApplicationRecord / ActiveRecord::Base.
	detectRubyORMModel(def.Node, src, id, name, filePath, result)
}

func (e *RubyExtractor) emitModule(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["mod.name"].Text
	def := m.Captures["mod.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": VisibilityPublic}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangHash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindPackage, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "ruby",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitMethod classifies a `def name` definition: direct child of a
// class's body_statement → method of that class; anything else → free
// top-level function. Mirrors the legacy qRbClassMethod +
// qRbMethod-fallback semantics.
func (e *RubyExtractor) emitMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["method.name"].Text
	def := m.Captures["method.def"]
	startLine1 := def.StartLine + 1

	className, _ := rubyDirectTypeParent(def.Node, src)
	if className != "" {
		id, ok := disambiguateID(seen, filePath+"::"+className+"."+name, startLine1)
		if !ok {
			return
		}
		meta := map[string]any{
			"receiver":   className,
			"signature":  "def " + name,
			"visibility": VisibilityPublic,
		}
		if doc := ExtractDocAbove(src, def.StartLine, DocLangHash); doc != "" {
			meta["doc"] = doc
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
			Language: "ruby", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
		})
		classID := filePath + "::" + className
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
		})
		return
	}

	id, ok := disambiguateID(seen, filePath+"::"+name, startLine1)
	if !ok {
		return
	}
	meta := map[string]any{
		"signature":  "def " + name,
		"visibility": VisibilityPublic,
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangHash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "ruby", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
}

// emitSingletonMethod mirrors the legacy qRbSingletonMethod pattern.
// `def self.foo` (and other `def receiver.foo` forms) is only
// meaningful as a class method — if we can't attribute it to an
// enclosing class, skip, matching legacy behaviour.
func (e *RubyExtractor) emitSingletonMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["singleton.name"].Text
	def := m.Captures["singleton.def"]
	startLine1 := def.StartLine + 1

	className := rubyDirectClassParent(def.Node, src)
	if className == "" {
		return
	}
	id, ok := disambiguateID(seen, filePath+"::"+className+"."+name, startLine1)
	if !ok {
		return
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "ruby", Meta: map[string]any{
			"receiver":  className,
			"signature": "def " + name,
		},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	classID := filePath + "::" + className
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
	})
}

func (e *RubyExtractor) emitRequire(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	method := m.Captures["req.method"].Text
	if method != "require" && method != "require_relative" {
		return
	}
	path := m.Captures["req.path"]
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + path.Text,
		Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
	})
}

func (e *RubyExtractor) emitConstant(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["const.name"].Text
	def := m.Captures["const.def"]
	if len(name) == 0 || !isUpperASCII(name[0]) {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindConstant, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "ruby",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// rubyDirectTypeParent returns the name of the class or module that
// directly owns def — def being a direct child of that type's
// body_statement — and whether the owner is a class (false for a
// module). Returns "", false for nested-in-method definitions or
// top-level defs, preserving the legacy free-function bucket. A method
// defined directly in a module body is a member of that module, the
// same way a class's method is a member of the class.
func rubyDirectTypeParent(def *sitter.Node, src []byte) (string, bool) {
	if def == nil {
		return "", false
	}
	parent := def.Parent()
	if parent == nil || parent.Type() != "body_statement" {
		return "", false
	}
	grand := parent.Parent()
	if grand == nil {
		return "", false
	}
	t := grand.Type()
	if t != "class" && t != "module" {
		return "", false
	}
	nameNode := grand.ChildByFieldName("name")
	if nameNode == nil {
		return "", false
	}
	return nameNode.Content(src), t == "class"
}

// rubyDirectClassParent returns the enclosing class name when the
// method/singleton_method is a direct child of a class's body_statement
// (mirrors the legacy nested qRbClassMethod / qRbSingletonMethod
// patterns). It is class-only: a `def self.foo` singleton method is
// only meaningful as a class method, so a module owner yields "".
func rubyDirectClassParent(def *sitter.Node, src []byte) string {
	if name, isClass := rubyDirectTypeParent(def, src); isClass {
		return name
	}
	return ""
}

// railsCallbackMethods enumerates the Rails controller macros that
// bind callbacks to actions. `skip_*` is intentionally excluded —
// it removes an inherited binding, and correctly honouring it would
// require parent-class tracking that's out of scope for the first
// pass. The negative-space impact is small; the positive binding
// from the parent class still surfaces as an edge.
var railsCallbackMethods = map[string]struct{}{
	"before_action": {},
	"after_action":  {},
	"around_action": {},
	"before_filter": {},
	"after_filter":  {},
	"around_filter": {},
}

func emitRailsCallbacks(root *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	// Walk every class body looking for top-level call expressions
	// whose method identifier matches a callback macro.
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "class" {
			nameNode := n.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			className := nameNode.Content(src)
			classID := filePath + "::" + className

			// Actions = instance methods of this class. Build a quick
			// map from method name to node ID so callbacks can be
			// resolved locally; avoids the resolver pass entirely for
			// this synthetic edge.
			methodIDs := make(map[string]string)
			var bodyStatements *sitter.Node
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				c := n.NamedChild(i)
				if c != nil && c.Type() == "body_statement" {
					bodyStatements = c
					break
				}
			}
			if bodyStatements == nil {
				return
			}
			// Collect methods first so callback macros can resolve
			// symbol names to concrete IDs.
			for i, _nc := 0, int(bodyStatements.NamedChildCount()); i < _nc; i++ {
				c := bodyStatements.NamedChild(i)
				if c == nil {
					continue
				}
				if c.Type() == "method" || c.Type() == "singleton_method" {
					nn := c.ChildByFieldName("name")
					if nn == nil {
						continue
					}
					name := nn.Content(src)
					methodIDs[name] = filePath + "::" + className + "." + name
				}
			}
			// First pass: collect every callback method named anywhere
			// in the class's before/after/around macros. These must be
			// excluded from the action set of EVERY macro — otherwise
			// `before_action :a; before_action :b` ends up binding a
			// to guard b and vice versa.
			allCallbacks := make(map[string]struct{})
			for i, _nc := 0, int(bodyStatements.NamedChildCount()); i < _nc; i++ {
				c := bodyStatements.NamedChild(i)
				if c == nil || c.Type() != "call" {
					continue
				}
				methodNode := c.ChildByFieldName("method")
				if methodNode == nil {
					continue
				}
				if _, ok := railsCallbackMethods[methodNode.Content(src)]; !ok {
					continue
				}
				args := c.ChildByFieldName("arguments")
				if args == nil {
					continue
				}
				for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
					arg := args.NamedChild(i)
					if arg != nil && arg.Type() == "simple_symbol" {
						allCallbacks[strings.TrimPrefix(arg.Content(src), ":")] = struct{}{}
					}
				}
			}
			// Second pass: emit edges.
			for i, _nc := 0, int(bodyStatements.NamedChildCount()); i < _nc; i++ {
				c := bodyStatements.NamedChild(i)
				if c == nil || c.Type() != "call" {
					continue
				}
				methodNode := c.ChildByFieldName("method")
				if methodNode == nil {
					continue
				}
				macro := methodNode.Content(src)
				if _, ok := railsCallbackMethods[macro]; !ok {
					continue
				}
				args := c.ChildByFieldName("arguments")
				if args == nil {
					continue
				}
				emitRailsCallbackEdges(args, src, filePath, int(c.StartPoint().Row)+1, classID, className, methodIDs, allCallbacks, macro, result)
			}
			return
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
}

// emitRailsCallbackEdges pulls symbol args out of a callback macro call,
// applies only:/except: filters against the class's action methods,
// and emits one EdgeCalls per (action, callback) pair. Class-level
// callbacks without only:/except: fan out to every action.
func emitRailsCallbackEdges(args *sitter.Node, src []byte, filePath string, line int, classID, className string, methodIDs map[string]string, allCallbacks map[string]struct{}, macro string, result *parser.ExtractionResult) {
	var callbackSyms []string
	onlyFilter := map[string]struct{}{}
	exceptFilter := map[string]struct{}{}
	hasOnly := false
	hasExcept := false
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		arg := args.NamedChild(i)
		if arg == nil {
			continue
		}
		switch arg.Type() {
		case "simple_symbol":
			// `:name` — the most common form.
			sym := strings.TrimPrefix(arg.Content(src), ":")
			callbackSyms = append(callbackSyms, sym)
		case "pair":
			// `only: :show` or `except: [:a, :b]`.
			keyNode := arg.ChildByFieldName("key")
			valNode := arg.ChildByFieldName("value")
			if keyNode == nil || valNode == nil {
				continue
			}
			key := strings.TrimSuffix(strings.TrimPrefix(keyNode.Content(src), ":"), ":")
			target := &onlyFilter
			set := &hasOnly
			switch key {
			case "only":
				// use default onlyFilter
			case "except":
				target = &exceptFilter
				set = &hasExcept
			default:
				continue
			}
			for _, sym := range collectRubySymbols(valNode, src) {
				(*target)[sym] = struct{}{}
			}
			if len(*target) > 0 {
				*set = true
			}
		case "hash":
			// Older Ruby fat-comma syntax (`only => :show`). Rare in
			// modern Rails; skip for simplicity.
		}
	}
	if len(callbackSyms) == 0 {
		return
	}

	// Resolve the actions this macro applies to.
	var applyTo []string
	for name := range methodIDs {
		if hasOnly {
			if _, ok := onlyFilter[name]; !ok {
				continue
			}
		}
		if hasExcept {
			if _, ok := exceptFilter[name]; ok {
				continue
			}
		}
		// Exclude ALL callback methods — a before_action can never
		// guard another before_action's method (Rails fires them all
		// sequentially, each bound to *actions*, not to each other).
		if _, isCallback := allCallbacks[name]; isCallback {
			continue
		}
		applyTo = append(applyTo, name)
	}
	if len(applyTo) == 0 {
		return
	}
	for _, cb := range callbackSyms {
		target := methodIDs[cb]
		if target == "" {
			// Inherited callback (defined on a parent class). Emit
			// an unresolved:: target and let the resolver find it by
			// name — works when the parent is in the same repo.
			target = "unresolved::" + cb
		}
		for _, action := range applyTo {
			actionID := methodIDs[action]
			if actionID == "" {
				continue
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     actionID,
				To:       target,
				Kind:     graph.EdgeCalls,
				FilePath: filePath,
				Line:     line,
				Meta: map[string]any{
					"dispatch_macro": macro,
					"rails_callback": cb,
				},
			})
		}
	}
	_ = classID
	_ = className
}

// collectRubySymbols gathers bare symbol tokens from an expression that
// may be a single symbol (`:foo`) or an array of them (`[:a, :b]`).
func collectRubySymbols(n *sitter.Node, src []byte) []string {
	var out []string
	switch n.Type() {
	case "simple_symbol":
		out = append(out, strings.TrimPrefix(n.Content(src), ":"))
	case "array":
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			c := n.NamedChild(i)
			if c != nil && c.Type() == "simple_symbol" {
				out = append(out, strings.TrimPrefix(c.Content(src), ":"))
			}
		}
	}
	return out
}

func isUpperASCII(b byte) bool {
	return b >= 'A' && b <= 'Z'
}

// Ensure RubyExtractor satisfies the Extractor interface at compile time.
var _ parser.Extractor = (*RubyExtractor)(nil)
