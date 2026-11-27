package contracts

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// ThriftExtractor detects Apache Thrift IDL service definitions and
// emits one provider contract per declared service function. The
// consumer side rides on the generated-stub patterns the gRPC
// extractor already recognises (`New<Service>Client(`), so the
// matcher's canonical-name join pairs thrift IDL providers with code
// that calls the generated client.
type ThriftExtractor struct{}

var (
	// namespace go shared / namespace java com.example.shared /
	// namespace * everything
	thriftNamespaceRe = regexp.MustCompile(`(?m)^\s*namespace\s+([\w.*]+)\s+([\w.]+)`)
	// service Calculator extends shared.SharedService {
	thriftServiceRe = regexp.MustCompile(`(?m)^\s*service\s+(\w+)(?:\s+extends\s+([\w.]+))?\s*\{`)
	// One function declaration inside a service block:
	//   void ping(),
	//   i32 add(1:i32 num1, 2:i32 num2),
	//   oneway void zip()
	//   list<Item> fetch(1: string id) throws (1: NotFound nf);
	// Groups: 1 = oneway (or ""), 2 = return type (incl. container
	// generics), 3 = function name.
	thriftFunctionRe = regexp.MustCompile(`(?m)^\s*(oneway\s+)?([\w.]+(?:\s*<[^>{}]*>)?)\s+(\w+)\s*\(`)
)

// thriftNamespacePreference orders the namespace scopes used to pick
// the single Meta["package"] value when a file declares several. "*"
// applies to every generator, so it wins; after that the order is an
// arbitrary-but-stable language preference.
var thriftNamespacePreference = []string{"*", "go", "java", "py", "python", "js", "rb", "cpp"}

func (e *ThriftExtractor) SupportedLanguages() []string {
	// "thrift" matches the parser registry's Language() for .thrift
	// files (see internal/parser/languages forest registrations).
	return []string{"thrift"}
}

func (e *ThriftExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	if !strings.HasSuffix(filePath, ".thrift") {
		return nil
	}
	var out []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	namespaces := make(map[string]string)
	for _, m := range thriftNamespaceRe.FindAllStringSubmatch(text, -1) {
		if _, exists := namespaces[m[1]]; !exists {
			namespaces[m[1]] = m[2]
		}
	}
	pkg := pickThriftNamespace(namespaces)

	for _, sMatch := range thriftServiceRe.FindAllStringSubmatchIndex(text, -1) {
		serviceName := text[sMatch[2]:sMatch[3]]
		// sMatch[1] points just past the `{`; bound the function scan
		// to this service's block so sibling services stay separate.
		blockEnd := matchCloseBrace(text, sMatch[1])
		if blockEnd < 0 {
			blockEnd = len(text)
		}
		blockStart := sMatch[1]
		block := text[blockStart:blockEnd]
		qualService := serviceName
		if pkg != "" {
			qualService = pkg + "." + serviceName
		}

		for _, fm := range thriftFunctionRe.FindAllStringSubmatchIndex(block, -1) {
			oneway := fm[2] >= 0
			retType := strings.TrimSpace(block[fm[4]:fm[5]])
			name := block[fm[6]:fm[7]]
			// Filter declarations the loose line regex can't tell
			// apart from functions: keyword-led lines are struct /
			// enum / typedef bodies that only appear in malformed
			// files, but cheap to guard anyway.
			if isThriftKeyword(retType) && retType != "void" {
				continue
			}
			absOffset := blockStart + fm[0]
			line := lineNumber(lines, absOffset)

			meta := map[string]any{
				"service":   serviceName,
				"method":    name,
				"canonical": qualService + "/" + name,
			}
			if pkg != "" {
				meta["package"] = pkg
			}
			if len(namespaces) > 0 {
				meta["namespaces"] = namespaces
			}
			if oneway {
				meta["oneway"] = true
			}
			if retType != "void" {
				meta["response_type"] = retType
				meta["schema_source"] = "extracted"
			} else {
				meta["schema_source"] = "none"
			}
			// fm[7] points at the function name's end; the `(`
			// follows after optional whitespace. Balance-scan the
			// argument list and record each field as "name:type".
			if args := thriftArgList(block, fm[7]); len(args) > 0 {
				meta["args"] = args
			}

			out = append(out, Contract{
				ID:         fmt.Sprintf("thrift::%s::%s", serviceName, name),
				Type:       ContractThrift,
				Role:       RoleProvider,
				FilePath:   filePath,
				Line:       line,
				Meta:       meta,
				Confidence: 0.95,
			})
		}
	}

	return out
}

// pickThriftNamespace selects the Meta["package"] value from the
// declared namespaces, preferring the generator-agnostic "*" scope,
// then a stable per-language order, then the lexicographically first scope.
func pickThriftNamespace(namespaces map[string]string) string {
	for _, scope := range thriftNamespacePreference {
		if ns, ok := namespaces[scope]; ok {
			return ns
		}
	}
	var first string
	var found bool
	for scope := range namespaces {
		if !found || scope < first {
			first, found = scope, true
		}
	}
	return namespaces[first]
}

// thriftArgList parses the parenthesised field list that starts after
// nameEnd (the byte offset just past the function name) and returns
// one "name:type" entry per field. Thrift fields look like
// `1: required i32 num1` — the numeric id and requiredness qualifier
// are stripped, the declared type and name kept.
func thriftArgList(block string, nameEnd int) []string {
	open := strings.Index(block[nameEnd:], "(")
	if open < 0 {
		return nil
	}
	openEnd := nameEnd + open + 1
	closeAt := matchCloseParen(block, openEnd)
	if closeAt < 0 {
		return nil
	}
	var out []string
	for _, raw := range splitTopLevelArgs(block[openEnd:closeAt]) {
		field := strings.TrimSpace(raw)
		if field == "" {
			continue
		}
		// Strip the leading `N:` field id.
		if colon := strings.Index(field, ":"); colon >= 0 && isThriftFieldID(field[:colon]) {
			field = strings.TrimSpace(field[colon+1:])
		}
		// Strip requiredness qualifiers.
		for _, q := range []string{"required ", "optional "} {
			field = strings.TrimPrefix(field, q)
		}
		// Drop a default value (`= 42`).
		if eq := strings.Index(field, "="); eq >= 0 {
			field = strings.TrimSpace(field[:eq])
		}
		// What remains is `<type> <name>` with the type possibly
		// containing generics and spaces (map<i32, string>). The name
		// is the final identifier.
		if sp := strings.LastIndexAny(field, " \t>"); sp >= 0 && sp+1 < len(field) {
			name := strings.TrimSpace(field[sp+1:])
			typ := strings.TrimSpace(field[:sp+1])
			if name != "" && typ != "" {
				out = append(out, name+":"+typ)
				continue
			}
		}
		out = append(out, field)
	}
	return out
}

// isThriftFieldID reports whether s is a numeric thrift field id (the
// `1` in `1: i32 num1`).
func isThriftFieldID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isThriftKeyword reports whether the token is a thrift declaration
// keyword that cannot be a function return type.
func isThriftKeyword(s string) bool {
	switch s {
	case "struct", "enum", "union", "exception", "typedef", "const",
		"service", "include", "namespace", "throws", "void":
		return true
	}
	return false
}
