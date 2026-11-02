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
	missing := false
	switch p.Query.Op {
	case "id":
		missing = p.Answer.Missing
	case "name", "file":
		missing = len(p.Answer.Nodes) == 0
	}
	return queryLatency{QueryID: p.Query.ID, Operation: p.Query.Op, Kind: p.Query.Kind, NS: p.NS, Warmup: p.Warmup, Missing: missing, Status: check.Status}, nil
}
