package contracts

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestResolveTypeInFileEmptyAndQualifiedNames(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "types.go::Item", Name: "Item", Kind: graph.KindType},
		{ID: "types.go::Empty", Kind: graph.KindType},
	}
	for _, tc := range []struct {
		name string
		want string
	}{
		{"*pkg.Item ", "types.go::Item"},
		{"pkg.", "types.go::Empty"},
		{"* ", ""},
		{"pkg.Absent", "Absent"},
	} {
		if got := resolveTypeInFile(tc.name, nodes); got != tc.want {
			t.Errorf("resolveTypeInFile(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Each test builds a minimal source file with a route and a handler,
// asks the extractor for contracts, and asserts on the schema-shape
// Meta keys. We only check the enriched fields — framework-routing
// coverage lives in http_test.go.

func TestHTTPEnrich_Go_NetHTTP_Stdlib_FullSchema(t *testing.T) {
	src := []byte(`package api

import (
	"encoding/json"
	"net/http"
)

type CreateReq struct {
	Name string ` + "`json:\"name\"`" + `
}

type CreateResp struct {
	ID string ` + "`json:\"id\"`" + `
}

func register(mux *http.ServeMux) {
	mux.HandleFunc("POST /users", createUser)
}

func createUser(w http.ResponseWriter, r *http.Request) {
	var req CreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := r.URL.Query().Get("tenant")
	_ = name
	resp := CreateResp{ID: "x"}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}
`)

	nodes := []*graph.Node{
		{ID: "pkg/api.go::register", Name: "register", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 15, EndLine: 17},
		{ID: "pkg/api.go::createUser", Name: "createUser", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 19, EndLine: 30},
		{ID: "pkg/api.go::CreateReq", Name: "CreateReq", Kind: graph.KindType, FilePath: "pkg/api.go", StartLine: 7, EndLine: 9},
		{ID: "pkg/api.go::CreateResp", Name: "CreateResp", Kind: graph.KindType, FilePath: "pkg/api.go", StartLine: 11, EndLine: 13},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.go", src, nodes, nil)

	c := findContract(t, cs, "http::POST::/users", RoleProvider)
	assertMetaString(t, c, "request_type", "pkg/api.go::CreateReq")
	assertMetaString(t, c, "response_type", "pkg/api.go::CreateResp")
	assertMetaStrings(t, c, "query_params", []string{"tenant"})
	assertMetaInts(t, c, "status_codes", []int{201, 400})
	assertMetaString(t, c, "schema_source", "extracted")
}

func TestHTTPEnrich_Go_Gin_RequestAndResponse(t *testing.T) {
	src := []byte(`package api

import "github.com/gin-gonic/gin"

type LoginReq struct{ Email string }
type LoginResp struct{ Token string }

func register(r *gin.Engine) {
	r.POST("/login", login)
}

func login(c *gin.Context) {
	var req LoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	resp := LoginResp{Token: "t"}
	c.JSON(200, resp)
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.go::register", Name: "register", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 7, EndLine: 9},
		{ID: "pkg/api.go::login", Name: "login", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 11, EndLine: 20},
		{ID: "pkg/api.go::LoginReq", Name: "LoginReq", Kind: graph.KindType, FilePath: "pkg/api.go", StartLine: 5, EndLine: 5},
		{ID: "pkg/api.go::LoginResp", Name: "LoginResp", Kind: graph.KindType, FilePath: "pkg/api.go", StartLine: 6, EndLine: 6},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.go", src, nodes, nil)

	c := findContract(t, cs, "http::POST::/login", RoleProvider)
	assertMetaString(t, c, "request_type", "pkg/api.go::LoginReq")
	assertMetaString(t, c, "response_type", "pkg/api.go::LoginResp")
	assertMetaInts(t, c, "status_codes", []int{200, 400})
	assertMetaString(t, c, "schema_source", "extracted")
}

func TestHTTPEnrich_Go_Fiber_Binding(t *testing.T) {
	src := []byte(`package api

import "github.com/gofiber/fiber/v2"

type CreateTuckReq struct{ Body string }

func register(app *fiber.App) {
	app.POST("/v1/tucks", createTuck)
}

func createTuck(c *fiber.Ctx) error {
	var req CreateTuckReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": err.Error()})
	}
	limit := c.Query("limit")
	_ = limit
	return c.JSON(req)
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.go::register", Name: "register", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 7, EndLine: 9},
		{ID: "pkg/api.go::createTuck", Name: "createTuck", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 11, EndLine: 19},
		{ID: "pkg/api.go::CreateTuckReq", Name: "CreateTuckReq", Kind: graph.KindType, FilePath: "pkg/api.go", StartLine: 5, EndLine: 5},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.go", src, nodes, nil)

	c := findContract(t, cs, "http::POST::/v1/tucks", RoleProvider)
	assertMetaString(t, c, "request_type", "pkg/api.go::CreateTuckReq")
	assertMetaStrings(t, c, "query_params", []string{"limit"})
	assertMetaInts(t, c, "status_codes", []int{400})
	// response is the same type as request in this fixture.
	assertMetaString(t, c, "schema_source", "extracted")
}

// Map-envelope response recursion still resolves when the inner
// identifier has a syntactic type we can read (Stage 1 scope).
// Cases where the inner identifier comes from a method-call return
// (e.g. `workspaces, err := h.svc.List(...)`) belong to the
// graph-aware `resolveCallReturnTypes` post-pass covered in the
// indexer test suite.
func TestHTTPEnrich_Go_RespondJSONEnvelope_SyntacticInner(t *testing.T) {
	src := []byte(`package api

import "net/http"

type Workspace struct { ID string }

func register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces", h.ListWorkspaces)
}

func (h *Handler) ListWorkspaces(w http.ResponseWriter, r *http.Request) {
	ws := &Workspace{}
	respondJSON(w, http.StatusOK, map[string]interface{}{"data": ws})
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.go::register", Name: "register", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 7, EndLine: 9},
		{ID: "pkg/api.go::Handler.ListWorkspaces", Name: "ListWorkspaces", Kind: graph.KindMethod, FilePath: "pkg/api.go", StartLine: 11, EndLine: 14},
		{ID: "pkg/api.go::Workspace", Name: "Workspace", Kind: graph.KindType, FilePath: "pkg/api.go", StartLine: 5, EndLine: 5},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.go", src, nodes, nil)
	c := findContract(t, cs, "http::GET::/v1/workspaces", RoleProvider)

	assertMetaString(t, c, "response_type", "pkg/api.go::Workspace")
	assertMetaInts(t, c, "status_codes", []int{200})
	assertMetaString(t, c, "schema_source", "extracted")
}

// Multi-key envelope: every key surfaces on response_envelope, with
// the source expression preserved per row. The keys whose values
// resolve to a typed binding get a "type"; the rest carry just the
// expression, which is still vastly more useful than the raw
// WriteJSON call previously stamped onto response_expr.
// Regression: a value like `[]any{evt1, evt2}` used to be captured
// by the multi-key envelope regex as `[]any{` (truncated at the
// first inner `}`), producing a malformed expr field in the
// dashboard. The brace/bracket-balanced splitter keeps the value
// expression intact and recognises the slice composite literal.
func TestHTTPEnrich_Go_EnvelopeValueIsNestedSliceLiteral(t *testing.T) {
	src := []byte(`package api

import "net/http"

type Event struct{ Name string }

func register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/activity", logActivity)
}

func logActivity(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{"events": []any{Event{}, Event{}}})
}

func WriteJSON(w http.ResponseWriter, code int, body any) {}
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.go::register", Name: "register", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 7, EndLine: 9},
		{ID: "pkg/api.go::logActivity", Name: "logActivity", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 11, EndLine: 13},
		{ID: "pkg/api.go::Event", Name: "Event", Kind: graph.KindType, FilePath: "pkg/api.go", StartLine: 5, EndLine: 5},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.go", src, nodes, nil)
	c := findContract(t, cs, "http::GET::/v1/activity", RoleProvider)

	env, ok := c.Meta["response_envelope"].([]map[string]any)
	if !ok || len(env) != 1 {
		t.Fatalf("response_envelope = %#v, want one row", c.Meta["response_envelope"])
	}
	row := env[0]
	expr, _ := row["expr"].(string)
	if expr == "" || strings.HasSuffix(expr, "{") {
		t.Errorf("expr is truncated: %q", expr)
	}
	if !strings.Contains(expr, "[]any{") || !strings.HasSuffix(expr, "}") {
		t.Errorf("expr missing the full literal; got %q", expr)
	}
	if got, _ := row["type"].(string); got == "" {
		t.Errorf("type empty for inline slice literal; want a recognised element type")
	}
	if r, _ := row["repeated"].(bool); !r {
		t.Errorf("repeated=false; want true for []any{...}")
	}
}

func TestHTTPEnrich_Go_RespondJSONEnvelope_MultiKey(t *testing.T) {
	src := []byte(`package api

import "net/http"

type FileEntry struct{ Path string }

func register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/overlay/sessions/{id}/files", h.handleOverlayList)
}

func (h *Handler) handleOverlayList(w http.ResponseWriter, r *http.Request) {
	out := []FileEntry{}
	total := 0
	WriteJSON(w, http.StatusOK, map[string]any{"files": out, "total": total})
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.go::register", Name: "register", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 7, EndLine: 9},
		{ID: "pkg/api.go::Handler.handleOverlayList", Name: "handleOverlayList", Kind: graph.KindMethod, FilePath: "pkg/api.go", StartLine: 11, EndLine: 15},
		{ID: "pkg/api.go::FileEntry", Name: "FileEntry", Kind: graph.KindType, FilePath: "pkg/api.go", StartLine: 5, EndLine: 5},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.go", src, nodes, nil)
	c := findContract(t, cs, "http::GET::/v1/overlay/sessions/{p1}/files", RoleProvider)

	env, ok := c.Meta["response_envelope"].([]map[string]any)
	if !ok || len(env) != 2 {
		t.Fatalf("response_envelope = %#v, want 2 rows", c.Meta["response_envelope"])
	}
	byName := map[string]map[string]any{}
	for _, row := range env {
		name, _ := row["name"].(string)
		byName[name] = row
	}
	if files := byName["files"]; files == nil {
		t.Errorf("envelope missing 'files' row; got %v", byName)
	} else if expr, _ := files["expr"].(string); expr != "out" {
		t.Errorf("envelope['files'].expr = %q, want \"out\"", expr)
	}
	if total := byName["total"]; total == nil {
		t.Errorf("envelope missing 'total' row; got %v", byName)
	} else if expr, _ := total["expr"].(string); expr != "total" {
		t.Errorf("envelope['total'].expr = %q, want \"total\"", expr)
	}
	// The previous behavior stamped the entire WriteJSON(...) call
	// as response_expr. With the envelope present we keep just the
	// value literal so the raw view is short.
	if got, _ := c.Meta["response_expr"].(string); strings.HasPrefix(got, "WriteJSON(") {
		t.Errorf("response_expr leaks the full WriteJSON wrapper: %q", got)
	}
}

func TestHTTPEnrich_Go_PathParams_AlwaysPresent(t *testing.T) {
	src := []byte(`package api

import "net/http"

func register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces/{wid}/tags/{id}", getTag)
}

func getTag(w http.ResponseWriter, r *http.Request) {}
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.go::register", Name: "register", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 5, EndLine: 7},
		{ID: "pkg/api.go::getTag", Name: "getTag", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 9, EndLine: 9},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.go", src, nodes, nil)

	c := findContract(t, cs, "http::GET::/v1/workspaces/{p1}/tags/{p2}", RoleProvider)
	// path_params are derived from the normalised template (positional
	// names). The user-written {wid} / {id} slots are rewritten to
	// {p1} / {p2} so cross-repo contract IDs match even when provider
	// and consumer teams picked different names.
	assertMetaStrings(t, c, "path_params", []string{"p1", "p2"})
}

func TestHTTPEnrich_Go_UnresolvedType_PartialSchema(t *testing.T) {
	src := []byte(`package api

import (
	"encoding/json"
	"net/http"
)

func register(mux *http.ServeMux) {
	mux.HandleFunc("POST /x", createX)
}

func createX(w http.ResponseWriter, r *http.Request) {
	var req UnknownThing
	_ = json.NewDecoder(r.Body).Decode(&req)
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.go::register", Name: "register", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 8, EndLine: 10},
		{ID: "pkg/api.go::createX", Name: "createX", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 12, EndLine: 15},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.go", src, nodes, nil)

	c := findContract(t, cs, "http::POST::/x", RoleProvider)
	// Type name wasn't in the file's node list, so we keep the bare
	// name and flag the meta so a later module-wide pass can resolve
	// it.
	assertMetaString(t, c, "request_type", "UnknownThing")
	assertMetaString(t, c, "schema_source", "extracted")
}

func TestHTTPEnrich_Go_Consumer_MarshalAndDecode(t *testing.T) {
	src := []byte(`package api

import (
	"bytes"
	"encoding/json"
	"net/http"
)

type CreateTuckReq struct{ Title string }
type CreateTuckResp struct{ ID string }

func callCreate() error {
	req := CreateTuckReq{Title: "a"}
	data, _ := json.Marshal(req)
	r, err := http.NewRequest("POST", "http://api/v1/tucks", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return err
	}
	var out CreateTuckResp
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return nil
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.go::callCreate", Name: "callCreate", Kind: graph.KindFunction, FilePath: "pkg/api.go", StartLine: 11, EndLine: 25},
		{ID: "pkg/api.go::CreateTuckReq", Name: "CreateTuckReq", Kind: graph.KindType, FilePath: "pkg/api.go", StartLine: 9, EndLine: 9},
		{ID: "pkg/api.go::CreateTuckResp", Name: "CreateTuckResp", Kind: graph.KindType, FilePath: "pkg/api.go", StartLine: 10, EndLine: 10},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.go", src, nodes, nil)

	c := findContract(t, cs, "http::POST::/v1/tucks", RoleConsumer)
	assertMetaString(t, c, "request_type", "pkg/api.go::CreateTuckReq")
	assertMetaString(t, c, "response_type", "pkg/api.go::CreateTuckResp")
	assertMetaString(t, c, "schema_source", "extracted")
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func findContract(t *testing.T, cs []Contract, id string, role Role) Contract {
	t.Helper()
	for _, c := range cs {
		if c.ID == id && c.Role == role {
			return c
		}
	}
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, string(c.Role)+" "+c.ID)
	}
	t.Fatalf("contract %s (%s) not found. have: %v", id, role, ids)
	return Contract{}
}

func assertMetaString(t *testing.T, c Contract, key, want string) {
	t.Helper()
	got, ok := c.Meta[key].(string)
	if !ok {
		t.Errorf("meta[%q] missing or not string on %s (got %T = %v)", key, c.ID, c.Meta[key], c.Meta[key])
		return
	}
	if got != want {
		t.Errorf("meta[%q] on %s = %q, want %q", key, c.ID, got, want)
	}
}

func assertMetaStrings(t *testing.T, c Contract, key string, want []string) {
	t.Helper()
	got, ok := c.Meta[key].([]string)
	if !ok {
		t.Errorf("meta[%q] missing or wrong type on %s: %T", key, c.ID, c.Meta[key])
		return
	}
	if len(got) != len(want) {
		t.Errorf("meta[%q] on %s = %v, want %v", key, c.ID, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("meta[%q] on %s = %v, want %v", key, c.ID, got, want)
			return
		}
	}
}

func assertMetaInts(t *testing.T, c Contract, key string, want []int) {
	t.Helper()
	got, ok := c.Meta[key].([]int)
	if !ok {
		t.Errorf("meta[%q] missing or wrong type on %s: %T", key, c.ID, c.Meta[key])
		return
	}
	if len(got) != len(want) {
		t.Errorf("meta[%q] on %s = %v, want %v", key, c.ID, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("meta[%q] on %s = %v, want %v", key, c.ID, got, want)
			return
		}
	}
}
