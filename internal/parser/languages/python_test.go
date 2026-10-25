package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestPyExtractor_Function(t *testing.T) {
	src := []byte(`def greet(name):
    return f"Hello {name}"
`)
	e := NewPythonExtractor()
	result, err := e.Extract("app.py", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "greet", funcs[0].Name)
}

func TestPyExtractor_Class(t *testing.T) {
	src := []byte(`class UserService:
    def __init__(self):
        self.users = []

    def get_user(self, user_id):
        return None
`)
	e := NewPythonExtractor()
	result, err := e.Extract("service.py", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "UserService", types[0].Name)

	// Class methods are extracted as KindMethod, not KindFunction.
	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.Len(t, methods, 2) // __init__, get_user

	// No top-level functions.
	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	assert.Len(t, funcs, 0)

	// EdgeMemberOf edges link methods to the class.
	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	assert.Len(t, memberEdges, 2)
	for _, e := range memberEdges {
		assert.Equal(t, "service.py::UserService", e.To)
	}
}

func TestPyExtractor_Imports(t *testing.T) {
	src := []byte(`import os
from pathlib import Path
`)
	e := NewPythonExtractor()
	result, err := e.Extract("app.py", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 2)
}

func TestPyExtractor_TypeEnv_TypeHint(t *testing.T) {
	src := []byte(`
class UserService:
    def save(self):
        pass

def main():
    svc: UserService = get_service()
    svc.save()
`)
	e := NewPythonExtractor()
	result, err := e.Extract("app.py", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall, "expected a call edge to save")
	require.NotNil(t, saveCall.Meta, "expected Meta on save call edge")
	assert.Equal(t, "UserService", saveCall.Meta["receiver_type"])
}

func TestPyExtractor_TypeEnv_ClassConstructor(t *testing.T) {
	src := []byte(`
class Client:
    def connect(self):
        pass

def main():
    client = Client()
    client.connect()
`)
	e := NewPythonExtractor()
	result, err := e.Extract("app.py", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var connectCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "connect") {
			connectCall = c
			break
		}
	}
	require.NotNil(t, connectCall)
	require.NotNil(t, connectCall.Meta)
	assert.Equal(t, "Client", connectCall.Meta["receiver_type"])
}

func TestPyExtractor_TypeEnv_Chain(t *testing.T) {
	src := []byte(`
class Connection:
    def query(self) -> Result:
        return Result()

class Result:
    def first(self) -> User:
        return User()

class User:
    def save(self):
        pass

def main():
    conn = Connection()
    conn.query().first().save()
`)
	e := NewPythonExtractor()
	result, err := e.Extract("app.py", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall, "expected a call edge to save")
	require.NotNil(t, saveCall.Meta, "expected Meta on chained save call edge")
	assert.Equal(t, "User", saveCall.Meta["receiver_type"])
}

func TestPyExtractor_TypeEnv_Unknown(t *testing.T) {
	src := []byte(`
def get_service():
    return None

def main():
    svc = get_service()
    svc.process()
`)
	e := NewPythonExtractor()
	result, err := e.Extract("app.py", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var processCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "process") {
			processCall = c
			break
		}
	}
	require.NotNil(t, processCall)
	assert.NotContains(t, processCall.Meta, "receiver_type", "unknown type should not produce a receiver_type hint")
}

func TestPythonExtractor_FastAPIDepends(t *testing.T) {
	// Depends(target) in a parameter default (or Annotated[T, Depends(target)])
	// should produce a direct call edge from the handler to target, not
	// just to the generic Depends function. Without this pass,
	// callers(target) is empty for any DI-only factory.
	src := []byte(`
from fastapi import Depends
from typing import Annotated

def get_settings():
    return {"db": "x"}

def handler(settings: Annotated[dict, Depends(get_settings)]):
    return settings
`)
	e := NewPythonExtractor()
	result, err := e.Extract("app.py", src)
	require.NoError(t, err)

	var found *graph.Edge
	for _, c := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if c.Meta == nil {
			continue
		}
		if v, _ := c.Meta["via"].(string); v == "fastapi.Depends" {
			if strings.HasSuffix(c.To, "get_settings") {
				found = c
				break
			}
		}
	}
	require.NotNil(t, found, "expected a fastapi.Depends edge to get_settings")
	assert.Equal(t, "app.py::handler", found.From)
}

func TestPythonExtractor_DependsOnlyOnIdentifierArg(t *testing.T) {
	// Depends() with a non-identifier argument (lambda, attribute access)
	// shouldn't produce a bogus edge — we can only statically resolve
	// plain identifier targets.
	src := []byte(`
from fastapi import Depends

def handler(x = Depends(lambda: 42), y = Depends(obj.method)):
    return x
`)
	e := NewPythonExtractor()
	result, err := e.Extract("app.py", src)
	require.NoError(t, err)

	for _, c := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if c.Meta == nil {
			continue
		}
		if v, _ := c.Meta["via"].(string); v == "fastapi.Depends" {
			t.Fatalf("unexpected fastapi.Depends edge: %+v", c)
		}
	}
}

func TestPythonExtractor_DocAndVisibility(t *testing.T) {
	src := []byte(`def foo():
    """Foo does the thing.

    Some details.
    """
    return 1


def _helper():
    """Internal helper."""
    return 2


class Server:
    """The server."""

    def start(self):
        """Start it."""
        return None

    def _stop(self):
        return None
`)
	e := NewPythonExtractor()
	result, err := e.Extract("server.py", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	foo := byID["server.py::foo"]
	require.NotNil(t, foo)
	if foo.Meta["doc"] != "Foo does the thing." {
		t.Fatalf("foo.doc = %q", foo.Meta["doc"])
	}
	if foo.Meta["visibility"] != "public" {
		t.Fatalf("foo.vis = %q", foo.Meta["visibility"])
	}

	helper := byID["server.py::_helper"]
	require.NotNil(t, helper)
	if helper.Meta["visibility"] != "private" {
		t.Fatalf("_helper.vis = %q", helper.Meta["visibility"])
	}

	server := byID["server.py::Server"]
	require.NotNil(t, server)
	if server.Meta["doc"] != "The server." {
		t.Fatalf("Server.doc = %q", server.Meta["doc"])
	}
	if server.Meta["visibility"] != "public" {
		t.Fatalf("Server.vis = %q", server.Meta["visibility"])
	}

	start := byID["server.py::Server.start"]
	require.NotNil(t, start)
	if start.Meta["doc"] != "Start it." {
		t.Fatalf("Server.start.doc = %q", start.Meta["doc"])
	}

	stop := byID["server.py::Server._stop"]
	require.NotNil(t, stop)
	if stop.Meta["visibility"] != "private" {
		t.Fatalf("Server._stop.vis = %q", stop.Meta["visibility"])
	}
}

func TestPythonExtractor_AnnotationEdges(t *testing.T) {
	src := []byte(`from app import app
from functools import lru_cache

@app.route("/users/:id")
@lru_cache
def get_user(id):
    return {}

@dataclass
class User:
    name: str

@deprecated
def old_helper():
    pass
`)
	e := NewPythonExtractor()
	result, err := e.Extract("svc.py", src)
	require.NoError(t, err)

	annNames := map[string]bool{}
	for _, n := range result.Nodes {
		if v, _ := n.Meta["kind"].(string); v == "annotation" {
			annNames[n.Name] = true
		}
	}
	for _, want := range []string{"app.route", "lru_cache", "dataclass", "deprecated"} {
		if !annNames[want] {
			t.Fatalf("missing annotation node %q (got %v)", want, annNames)
		}
	}

	edges := map[string][]string{}
	argsByEdge := map[string]string{}
	for _, e := range result.Edges {
		if e.Kind != graph.EdgeAnnotated {
			continue
		}
		edges[e.From] = append(edges[e.From], e.To)
		if v, ok := e.Meta["args"].(string); ok {
			argsByEdge[e.From+"->"+e.To] = v
		}
	}

	if !pyContains(edges["svc.py::get_user"], "annotation::python::app.route") {
		t.Fatalf("missing app.route edge on get_user, got %v", edges["svc.py::get_user"])
	}
	if argsByEdge["svc.py::get_user->annotation::python::app.route"] != `"/users/:id"` {
		t.Fatalf("app.route args = %q", argsByEdge["svc.py::get_user->annotation::python::app.route"])
	}
	if !pyContains(edges["svc.py::User"], "annotation::python::dataclass") {
		t.Fatalf("missing dataclass edge on User, got %v", edges["svc.py::User"])
	}
}

func pyContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func TestPythonExtractor_ThrowsRaise(t *testing.T) {
	src := []byte(`def parse(s):
    if not s:
        raise ValueError("empty")
    if len(s) > 10:
        raise errors.TooLong()
    return int(s)


def silent():
    try:
        x = 1
    except Exception:
        pass
`)
	e := NewPythonExtractor()
	result, err := e.Extract("p.py", src)
	require.NoError(t, err)

	throws := edgesOfKind(result.Edges, graph.EdgeThrows)
	parseTargets := map[string]bool{}
	for _, e := range throws {
		if e.From == "p.py::parse" {
			parseTargets[e.To] = true
		}
	}
	assert.True(t, parseTargets["unresolved::ValueError"], "ValueError missing")
	assert.True(t, parseTargets["unresolved::TooLong"], "TooLong (attribute access) missing")

	for _, e := range throws {
		if e.From == "p.py::silent" {
			t.Fatalf("silent() doesn't raise; got %v", e)
		}
	}
}

func TestPythonExtractor_ImportNodes(t *testing.T) {
	// Note: relative imports (`from .local import x`) aren't covered
	// by the current import query — pre-existing limitation. The
	// import-node promotion only sees absolute paths today.
	src := []byte(`import os
import requests
from app import service
`)
	e := NewPythonExtractor()
	result, err := e.Extract("svc.py", src)
	require.NoError(t, err)

	imports := nodesOfKind(result.Nodes, graph.KindImport)
	require.GreaterOrEqual(t, len(imports), 3)

	byID := map[string]*graph.Node{}
	for _, n := range imports {
		byID[n.ID] = n
	}

	osNode := byID["svc.py::import::os"]
	require.NotNil(t, osNode)
	assert.Equal(t, false, osNode.Meta["is_external"], "os is stdlib")

	req := byID["svc.py::import::requests"]
	require.NotNil(t, req)
	assert.Equal(t, true, req.Meta["is_external"])
}

// Decorated methods (@property, @staticmethod, @contextmanager, ...) are
// wrapped in a decorated_definition node between the def and the class
// body. They must still classify as class members: two same-named
// decorated properties on two classes in one file used to collapse into
// bare colliding `<file>::<name>` IDs (httpx's Headers.encoding vs
// Response.encoding), so every consumer addressing the class-qualified
// ID silently landed on the wrong declaration.
func TestPyExtractor_DecoratedMethodsAreClassMembers(t *testing.T) {
	src := []byte(`class Headers:
    @property
    def encoding(self):
        return "ascii"

class Response:
    @property
    def encoding(self):
        return "utf-8"

    @staticmethod
    def build():
        return Response()

def caller():
    @wraps
    def inner():
        pass
    return inner
`)
	e := NewPythonExtractor()
	result, err := e.Extract("m.py", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	he := byID["m.py::Headers.encoding"]
	require.NotNil(t, he, "decorated property must get a class-qualified ID (got IDs: %v)", pyNodeIDs(result.Nodes))
	assert.Equal(t, graph.KindMethod, he.Kind)
	assert.Equal(t, 3, he.StartLine)
	assert.Equal(t, "Headers", he.Meta["receiver"])

	re := byID["m.py::Response.encoding"]
	require.NotNil(t, re, "same-named decorated property on a second class must stay distinct")
	assert.Equal(t, graph.KindMethod, re.Kind)
	assert.Equal(t, 8, re.StartLine)
	assert.Equal(t, "Response", re.Meta["receiver"])

	rb := byID["m.py::Response.build"]
	require.NotNil(t, rb, "@staticmethod def must classify as a method")
	assert.Equal(t, graph.KindMethod, rb.Kind)

	// A decorated function nested inside a function is NOT a class
	// member — it stays in the free-function bucket.
	in := byID["m.py::inner"]
	require.NotNil(t, in)
	assert.Equal(t, graph.KindFunction, in.Kind)

	// member_of edges link each decorated method to its class.
	memberTo := map[string]string{}
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeMemberOf) {
		memberTo[ed.From] = ed.To
	}
	assert.Equal(t, "m.py::Headers", memberTo["m.py::Headers.encoding"])
	assert.Equal(t, "m.py::Response", memberTo["m.py::Response.encoding"])
	assert.Equal(t, "m.py::Response", memberTo["m.py::Response.build"])
}

func pyNodeIDs(nodes []*graph.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

func TestPyExtractor_ImportedReceiverBindings(t *testing.T) {
	result, err := NewPythonExtractor().Extract("app.py", []byte("from pkg.image import Image as Picture, Other\n"))
	require.NoError(t, err)
	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 1)
	require.Equal(t, map[string]any{"Picture": "Image", "Other": "Other"}, imports[0].Meta["python_bindings"])
}
