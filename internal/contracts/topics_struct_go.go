package contracts

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

var _ StoreAwareExtractor = (*TopicExtractor)(nil)

// awsEndpointStructField maps an AWS request-struct field name to the broker it
// identifies. Only these two unambiguous SDK field names drive topic emission
// from a composite literal, keeping false positives near zero — a queue URL or
// topic ARN field has no non-messaging meaning.
var awsEndpointStructField = map[string]string{
	"QueueUrl": "sqs",
	"TopicArn": "sns",
}

// ExtractWithStore runs the regex provider / consumer detection and, for Go,
// an additional AST pass that resolves AWS-style request-struct endpoints
// (&sqs.SendMessageInput{QueueUrl: q} / &sns.PublishInput{TopicArn: t}) whose
// queue / topic is a const or variable the regex layer cannot see. The
// queue / topic value is resolved graph-wide through ResolveEndpointArg, so a
// const (including a cross-file const) resolves to its literal. Implements
// StoreAwareExtractor.
func (e *TopicExtractor) ExtractWithStore(
	filePath string,
	src []byte,
	nodes []*graph.Node,
	edges []*graph.Edge,
	tree *parser.ParseTree,
	store EndpointConstStore,
	repoPrefix string,
) []Contract {
	out := e.Extract(filePath, src, nodes, edges)
	if detectLanguage(filePath) != "go" {
		return out
	}
	if tree == nil {
		tree = ParseTreeForLang("go", src)
		defer tree.Release()
	}
	if tree == nil || tree.Tree() == nil {
		return out
	}
	fileNodes := filterFileNodes(filePath, nodes)
	sort.Slice(fileNodes, func(i, j int) bool {
		return fileNodes[i].StartLine < fileNodes[j].StartLine
	})
	out = append(out, detectGoEndpointStructTopics(tree.Tree().RootNode(), src, filePath, repoPrefix, store, fileNodes, len(out))...)
	return out
}

// detectGoEndpointStructTopics walks call_expressions for an argument that is
// an AWS-style request struct (&sqs.SendMessageInput{QueueUrl: q} /
// sns.PublishInput{TopicArn: t}) and emits a ContractTopic whose name is the
// resolved queue / topic. The endpoint value is resolved graph-wide via
// ResolveEndpointArg or a detached EndpointExpression (forRoute=false), so a const
// or cross-file-const queue resolves where the regex layer cannot. The role is
// inferred from the enclosing call's method name (receive / subscribe / consume
// → consumer; otherwise provider).
func detectGoEndpointStructTopics(root *sitter.Node, src []byte, filePath, repoPrefix string, store EndpointConstStore, fileNodes []*graph.Node, base int) []Contract {
	if root == nil {
		return nil
	}
	var out []Contract
	walkGoCallExprs(root, func(call *sitter.Node) {
		args := call.ChildByFieldName("arguments")
		if args == nil {
			return
		}
		role := topicRoleForMethod(goCallTrailingName(call, src))
		for _, arg := range namedChildren(args) {
			comp := compositeArgNode(arg)
			if comp == nil {
				continue
			}
			field, valNode := awsEndpointField(comp, src)
			if field == "" || valNode == nil {
				continue
			}
			broker := awsEndpointStructField[field]
			if capture, ok := store.(*EndpointFactCapture); ok {
				q := endpointExpression(valNode, src)
				if q.HasQuestion() {
					ln := int(call.StartPoint().Row) + 1
					c := Contract{
						Type: ContractTopic, Role: role, SymbolID: findEnclosingSymbol(fileNodes, ln),
						FilePath: filePath, Line: ln, Meta: map[string]any{"broker": broker}, Confidence: 0.8,
					}
					capture.add(EndpointContractQuestion{Position: base + len(out), Kind: EndpointTopic, Expression: q, Record: c})
					continue
				}
			}
			topic, ok := ResolveEndpointArg(valNode, src, filePath, repoPrefix, store, false)
			if !ok || topic == "" {
				continue
			}
			ln := int(call.StartPoint().Row) + 1
			out = append(out, Contract{
				ID:       fmt.Sprintf("topic::%s::%s", broker, topic),
				Type:     ContractTopic,
				Role:     role,
				SymbolID: findEnclosingSymbol(fileNodes, ln),
				FilePath: filePath,
				Line:     ln,
				Meta: map[string]any{
					"topic":  topic,
					"broker": broker,
				},
				Confidence: 0.8,
			})
		}
	})
	return out
}

// compositeArgNode returns the composite_literal underlying a call argument,
// unwrapping a leading & (unary_expression) or parentheses. Returns nil when
// the argument is not a composite literal.
func compositeArgNode(arg *sitter.Node) *sitter.Node {
	for arg != nil {
		switch arg.Type() {
		case "composite_literal":
			return arg
		case "unary_expression":
			// The operand field name is grammar-version-dependent; fall back
			// to the first named child (the `&` operator is anonymous).
			op := arg.ChildByFieldName("operand")
			if op == nil {
				op = firstNamedChild(arg)
			}
			arg = op
		case "parenthesized_expression":
			arg = firstNamedChild(arg)
		default:
			return nil
		}
	}
	return nil
}

// awsEndpointField returns the first composite-literal field whose bare name is
// a recognised AWS endpoint field (QueueUrl / TopicArn), with its value node.
func awsEndpointField(comp *sitter.Node, src []byte) (string, *sitter.Node) {
	body := compositeBodyNode(comp)
	if body == nil {
		return "", nil
	}
	for i := 0; i < int(body.NamedChildCount()); i++ {
		kv := body.NamedChild(i)
		if kv == nil || kv.Type() != "keyed_element" || kv.NamedChildCount() < 2 {
			continue
		}
		keyNode := unwrapLiteralElement(kv.NamedChild(0))
		valNode := unwrapLiteralElement(kv.NamedChild(1))
		if keyNode == nil || valNode == nil {
			continue
		}
		name := strings.TrimSpace(keyNode.Content(src))
		if _, ok := awsEndpointStructField[name]; ok {
			return name, valNode
		}
	}
	return "", nil
}

// goCallTrailingName returns the trailing method / function name of a call's
// function expression (client.SendMessage → "SendMessage", Publish → "Publish").
func goCallTrailingName(call *sitter.Node, src []byte) string {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Type() {
	case "selector_expression":
		if f := fn.ChildByFieldName("field"); f != nil {
			return f.Content(src)
		}
	case "identifier":
		return fn.Content(src)
	}
	return ""
}

// topicRoleForMethod classifies a pub/sub call method name as a consumer
// (receive / subscribe / consume / read) or, by default, a provider.
func topicRoleForMethod(method string) Role {
	m := strings.ToLower(method)
	switch {
	case strings.Contains(m, "receive"),
		strings.Contains(m, "subscribe"),
		strings.Contains(m, "consume"),
		strings.Contains(m, "read"):
		return RoleConsumer
	default:
		return RoleProvider
	}
}

func completeEndpointTopic(c *Contract, topic string) {
	c.ID = fmt.Sprintf("topic::%s::%s", c.Meta["broker"], topic)
	c.Meta["topic"] = topic
}
