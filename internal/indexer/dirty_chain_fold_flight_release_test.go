package indexer

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/storage"
)

// A compaction canceled before the fold copier joins the destination's build
// flight holds no flight; releasing the destination must not call a nil
// handle (a panic here would end the compaction goroutine and the daemon).
func TestCompactDirtyChainSteppedCanceledBeforeJoinReleasesNoFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &CheckoutCoordinator{}
	report := &DirtyChainCompaction{}
	got := c.compactDirtyChainStepped(ctx, CheckoutCycle{}, storage.ViewGeneration{}, storage.CheckoutRoute{}, report, func() {}, func() {})
	if got != "" {
		t.Fatalf("compactDirtyChainStepped = %q, want empty", got)
	}
	if !report.Canceled || report.Outcome != dirtyChainCompactionCanceled {
		t.Fatalf("report = canceled %v outcome %q, want canceled %q", report.Canceled, report.Outcome, dirtyChainCompactionCanceled)
	}
}
