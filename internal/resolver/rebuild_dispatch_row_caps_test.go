package resolver

import (
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildDispatchRowCaps(t *testing.T) {
	padding := strings.Repeat("x", 16<<20)
	for _, stage := range []struct {
		name, file, lang string
		handler, call    map[string]any
		nodeCap          bool
	}{
		{SynthSpringEvent, "Events.java", "java", map[string]any{"spring_listener_type": "Event"},
			map[string]any{"via": springEventVia, "spring_event_type": "Event"}, true},
		{SynthMediatR, "Dispatch.cs", "csharp", map[string]any{"mediatr_kind": "request", "mediatr_request_type": "Event"},
			map[string]any{"via": mediatrVia, "mediatr_kind": "request", "mediatr_request_type": "Event"}, true},
		{SynthLaravelEvent, "Events.php", "php", map[string]any{"laravel_listener_type": "Event"},
			map[string]any{"via": laravelEventVia, "laravel_event_type": "Event"}, false},
	} {
		t.Run(stage.name, func(t *testing.T) {
			selection, err := NewFrameworkSynthesizerSelection([]string{stage.name})
			require.NoError(t, err)
			rows := []string{"tagged Call", "unrelated Call"}
			if stage.nodeCap {
				rows = append(rows, "handler", "unrelated node")
			}
			for _, row := range rows {
				t.Run(row, func(t *testing.T) {
					handler := &graph.Node{ID: "handler", Name: "listen", Kind: graph.KindMethod,
						FilePath: stage.file, Language: stage.lang, Meta: stage.handler}
					call := &graph.Edge{From: "caller", To: "unresolved::old", Kind: graph.EdgeCalls,
						FilePath: stage.file, Line: 1, Meta: stage.call}
					nodes, edges := []*graph.Node{handler}, []*graph.Edge{call}
					var wantErr bool
					switch row {
					case "tagged Call":
						call.Context = padding
						wantErr = true
					case "unrelated Call":
						edges = append(edges, &graph.Edge{From: "caller", To: "other", Kind: graph.EdgeCalls,
							FilePath: stage.file, Line: 2, Context: padding})
					case "handler":
						handler.Meta = maps.Clone(stage.handler)
						handler.Meta["padding"] = padding
						wantErr = true
					case "unrelated node":
						nodes = append(nodes, &graph.Node{ID: "other", Kind: graph.KindMethod, Name: "unrelated",
							FilePath: stage.file, Language: stage.lang, Meta: map[string]any{"padding": padding}})
					}
					facts := rebuildEventFacts(stage.file, stage.lang, nodes, edges...)
					b := openRebuildGRPCBuild(t, facts)
					receipts := rebuildGRPCReceipts(facts)
					var err error
					var moves uint64
					switch stage.name {
					case SynthSpringEvent:
						stats, stageErr := RunRebuildSpringEvents(t.Context(), b, receipts, selection)
						moves, err = stats.Moves, stageErr
					case SynthMediatR:
						stats, stageErr := RunRebuildMediatR(t.Context(), b, receipts, selection)
						moves, err = stats.Moves, stageErr
					case SynthLaravelEvent:
						stats, stageErr := RunRebuildLaravelEvent(t.Context(), b, receipts, selection)
						moves, err = stats.Moves, stageErr
					}
					if wantErr {
						require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
						require.Zero(t, moves)
						return
					}
					require.NoError(t, err)
					require.Equal(t, uint64(1), moves)
				})
			}
		})
	}
}
