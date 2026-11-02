package runner

import (
	"fmt"
	"gxstudy/workload"
)

type queryLatencyKey struct{}
type queryLatencyOutput struct {
	dir    string
	result *Result
}
type queryLatency struct {
	QueryID   string `json:"query_id"`
	Operation string `json:"operation"`
	Kind      string `json:"kind"`
	NS        int64  `json:"elapsed_ns"`
	Warmup    bool   `json:"warmup"`
	Missing   bool   `json:"missing"`
	Status    string `json:"status"`
}

func latencyFromPending(p pendingCheck, check workload.Check) (queryLatency, error) {
	if p.NS <= 0 {
		return queryLatency{}, fmt.Errorf("supported query %s lacks positive engine elapsed_ns", p.Query.ID)
	}
	missing := lookupMissing(p.Query, p.Answer)
	return queryLatency{QueryID: p.Query.ID, Operation: p.Query.Op, Kind: p.Query.Kind, NS: p.NS, Warmup: p.Warmup, Missing: missing, Status: check.Status}, nil
}

type queryMinimumKey struct{}

func lookupMissing(query workload.Query, answer workload.Answer) bool {
	switch query.Op {
	case "id":
		return answer.Missing
	case "name", "file":
		return len(answer.Nodes) == 0
	default:
		return false
	}
}

// Only design-specific, judged strata receive repetition. Shared text paths
// remain observational. Empty means this operation is not judged here.
func sampleStratum(q workload.Query, a workload.Answer) string {
	switch q.Op {
	case "id", "name", "file":
		if lookupMissing(q, a) {
			return q.Op + "_miss"
		}
		return q.Op + "_hit"
	case "in", "multi_in":
		if q.Kind == "calls" {
			return q.Op + "_calls"
		}
	case "traverse2", "traverse3", "multi_name", "multi_traverse2", "multi_traverse3":
		return q.Op
	}
	return ""
}
func sampleRepetitions(stratum string, warmCount, minimum int) int {
	if stratum == "" || minimum <= 1 {
		return 1
	}
	return max(1, (minimum+max(1, warmCount)-1)/max(1, warmCount))
}
