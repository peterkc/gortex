package indexer

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
)

// eventBusBoundaries returns the configured event-bus boundaries for this
// indexer, converting config specs to the contracts package's cycle-free
// boundary type. The CODEGRAPH_EVENT_CONFIG env var (a JSON list, drop-in
// compatible with pygraph/tsgraph) overrides .gortex.yaml's index.event_bus
// when set and valid; GORTEX_EVENT_BUS_DISABLE=1 forces the bus off.
func (idx *Indexer) eventBusBoundaries() []contracts.EventBusBoundary {
	if policy := idx.frozenRebuildContractPolicy.Load(); policy != nil {
		return policy.eventBus
	}
	if os.Getenv("GORTEX_EVENT_BUS_DISABLE") == "1" {
		return nil
	}
	specs := idx.config.EventBus
	if env := strings.TrimSpace(os.Getenv("CODEGRAPH_EVENT_CONFIG")); env != "" {
		if parsed, ok := parseEventConfigJSON(env); ok {
			specs = parsed
		}
	}
	if len(specs) == 0 {
		return nil
	}
	out := make([]contracts.EventBusBoundary, 0, len(specs))
	for _, s := range specs {
		b := eventBusBoundaryFromSpec(s)
		if b.Name == "" || (b.Callee == "" && b.CalleePattern == "" && b.Decorator == "" && b.Interface == "") {
			continue // malformed spec: skip, never fail the index
		}
		out = append(out, b)
	}
	return out
}

func eventBusBoundaryFromSpec(s config.EventBusBoundarySpec) contracts.EventBusBoundary {
	calleePattern := s.CalleePattern
	if calleePattern == "" {
		calleePattern = s.HookPattern // tsgraph alias
	}
	return contracts.EventBusBoundary{
		Name:          s.Name,
		Type:          s.Type,
		Callee:        s.Callee,
		CalleePattern: calleePattern,
		Decorator:     s.Decorator,
		Interface:     s.Interface,
		TopicArg:      s.TopicArg,
		Guards:        s.Guards,
	}
}

// eventConfigJSONEntry is the CODEGRAPH_EVENT_CONFIG wire shape: each entry has
// a top-level name/type plus a nested `match` block (pygraph/tsgraph format).
type eventConfigJSONEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Match struct {
		Callee        string         `json:"callee"`
		CalleePattern string         `json:"callee_pattern"`
		HookPattern   string         `json:"hook_pattern"`
		Decorator     string         `json:"decorator"`
		Interface     string         `json:"interface"`
		TopicArg      string         `json:"topic_arg"`
		Args          map[string]any `json:"args"`
		Guards        bool           `json:"guards"`
	} `json:"match"`
}

// parseEventConfigJSON parses the env var into config specs. Returns ok=false
// (so the caller falls back to .gortex.yaml) on malformed JSON.
func parseEventConfigJSON(raw string) ([]config.EventBusBoundarySpec, bool) {
	var entries []eventConfigJSONEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, false
	}
	out := make([]config.EventBusBoundarySpec, 0, len(entries))
	for _, e := range entries {
		topicArg := e.Match.TopicArg
		if topicArg == "" {
			// pygraph stores arg extraction under match.args{<out>:<int|str>};
			// the topic/event_code/url key is the join key.
			topicArg = eventArgTopicKey(e.Match.Args)
		}
		out = append(out, config.EventBusBoundarySpec{
			Name:          e.Name,
			Type:          e.Type,
			Callee:        e.Match.Callee,
			CalleePattern: e.Match.CalleePattern,
			HookPattern:   e.Match.HookPattern,
			Decorator:     e.Match.Decorator,
			Interface:     e.Match.Interface,
			TopicArg:      topicArg,
			Guards:        e.Match.Guards,
		})
	}
	return out, true
}

// eventArgTopicKey picks the arg whose extracted value should key the
// producer↔consumer join from a pygraph-style match.args map, preferring
// topic/event_code/url. The value (positional index or kwarg name) becomes
// TopicArg.
func eventArgTopicKey(args map[string]any) string {
	for _, k := range []string{"topic", "event_code", "url", "channel", "subject"} {
		if v, ok := args[k]; ok {
			return argValueToTopicArg(v)
		}
	}
	return ""
}

func argValueToTopicArg(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// integer-valued positional index from JSON ("0", "1", ...)
		return strconv.Itoa(int(t))
	}
	return ""
}
