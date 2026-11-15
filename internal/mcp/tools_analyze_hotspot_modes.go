package mcp

import (
	"sort"
	"time"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// rerankHotspots applies K17's two alternate ranking modes on top of
// the analyzer's complexity score. The legacy mode "complexity" is
// pass-through; "novelty" and "directional" multiply the complexity
// score by a recency weight derived from the blame / releases
// substrate.
//
// Substrate dependency (missing novelty blame is reported as unavailable):
//
//	novelty     reads meta.last_authored.timestamp. Symbols touched
//	            within window_days get weight >0 (rising toward 1
//	            as the touch approaches "now"); untouched symbols
//	            get weight 0 and sort to the bottom.
//
//	directional reads meta.added_in.timestamp (the releases-pipeline
//	            stamp from `gortex enrich releases`). With
//	            direction="adds" the weight favours recently-added
//	            symbols; "stable" inverts to favour old additions.
//	            Symbols missing the metadata silently score 0.
//
// We don't fail when the meta is absent — the analyzer treats this
// as a soft ranker, not a strict filter, so callers get *some*
// ranking even on un-enriched graphs. The availability result only concerns
// novelty's blame sidecar; directional and complexity do not need it.
func rerankHotspots(entries []analysis.HotspotEntry, g graph.Store, mode, direction string, windowDays int) ([]analysis.HotspotEntry, bool) {
	if windowDays <= 0 {
		windowDays = 30
	}
	now := time.Now().UTC()
	window := time.Duration(windowDays) * 24 * time.Hour
	var blame map[string]graph.BlameEnrichment
	available := true
	if mode == "novelty" {
		blame, available = blameRowsByID(g)
	}

	weighted := make([]analysis.HotspotEntry, 0, len(entries))
	for _, e := range entries {
		n := g.GetNode(e.ID)
		if n == nil {
			continue
		}
		var weight float64
		switch mode {
		case "novelty":
			weight = noveltyWeight(blame, n, now, window)
		case "directional":
			weight = directionalWeight(n, now, window, direction)
		default:
			weight = 1.0
		}
		// Scale the complexity score so the existing fields stay
		// meaningful — the agent still sees fan_in / fan_out /
		// crossings; only the rank order shifts.
		e.ComplexityScore = e.ComplexityScore * weight
		weighted = append(weighted, e)
	}
	sort.Slice(weighted, func(i, j int) bool {
		if weighted[i].ComplexityScore != weighted[j].ComplexityScore {
			return weighted[i].ComplexityScore > weighted[j].ComplexityScore
		}
		return weighted[i].ID < weighted[j].ID
	})
	return weighted, available
}

// noveltyWeight returns 1.0 - days_since_last_authored / windowDays,
// clamped to [0, 1]. Symbols missing the meta return 0 — they sort
// to the bottom rather than getting a free "fully novel" pass.
func noveltyWeight(blame map[string]graph.BlameEnrichment, n *graph.Node, now time.Time, window time.Duration) float64 {
	ts := nodeLastAuthoredTime(blame, n)
	if ts.IsZero() {
		return 0
	}
	elapsed := now.Sub(ts)
	if elapsed < 0 {
		return 1.0
	}
	if elapsed >= window {
		return 0
	}
	return 1.0 - float64(elapsed)/float64(window)
}

// directionalWeight reads meta.added_in.timestamp. direction="adds"
// rewards recently-added symbols; "stable" rewards old additions.
// Empty / unknown direction defaults to "adds".
func directionalWeight(n *graph.Node, now time.Time, window time.Duration, direction string) float64 {
	ts := nodeAddedInTime(n)
	if ts.IsZero() {
		return 0
	}
	elapsed := now.Sub(ts)
	if elapsed < 0 {
		elapsed = 0
	}
	frac := float64(elapsed) / float64(window)
	if frac > 1 {
		frac = 1
	}
	switch direction {
	case "stable":
		return frac // older addition = higher
	default: // "adds"
		return 1.0 - frac // newer addition = higher
	}
}

// nodeLastAuthoredTime returns the meta.last_authored.timestamp as a
// time.Time, or zero when the field isn't populated. Blame writes
// the timestamp as a Unix int64; releases enrichment may write an
// RFC3339 string — we tolerate both.
func nodeLastAuthoredTime(blame map[string]graph.BlameEnrichment, n *graph.Node) time.Time {
	e, ok := lastAuthoredFrom(blame, n)
	if !ok || e.Timestamp == 0 {
		return time.Time{}
	}
	return time.Unix(e.Timestamp, 0)
}

// nodeAddedInTime returns meta.added_in.timestamp as a time.Time,
// or zero when absent. Same dual encoding as last_authored.
func nodeAddedInTime(n *graph.Node) time.Time {
	if n.Meta == nil {
		return time.Time{}
	}
	ai, ok := n.Meta["added_in"].(map[string]any)
	if !ok {
		return time.Time{}
	}
	return decodeMetaTimestamp(ai["timestamp"])
}

// decodeMetaTimestamp handles the int-seconds / float-seconds /
// RFC3339-string forms blame and releases enrichment produce. Empty
// or unknown shapes return zero.
func decodeMetaTimestamp(v any) time.Time {
	switch t := v.(type) {
	case int64:
		return time.Unix(t, 0).UTC()
	case int:
		return time.Unix(int64(t), 0).UTC()
	case float64:
		return time.Unix(int64(t), 0).UTC()
	case string:
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			return ts.UTC()
		}
	}
	return time.Time{}
}
