package mcp

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type analysisPruneTestStore struct {
	graph.AnalysisGenerationStore
	prune func(context.Context, int, int) error
}

func (s *analysisPruneTestStore) PruneAnalysisGenerations(ctx context.Context, keep, batch int) error {
	return s.prune(ctx, keep, batch)
}

func TestAnalysisGenerationPruneRetriesDeadlinesUntilComplete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Server{}
		t.Cleanup(s.DrainBackground)
		start := time.Now()
		// The prune goroutine appends while the test reads between fake-clock
		// sleeps, which do not order the two goroutines for the race detector.
		var mu sync.Mutex
		var calls []time.Duration
		attempts := func() []time.Duration {
			mu.Lock()
			defer mu.Unlock()
			return append([]time.Duration(nil), calls...)
		}
		writer := &analysisPruneTestStore{prune: func(ctx context.Context, keep, batch int) error {
			if keep != 2 || batch != 1000 {
				t.Errorf("keep=%d batch=%d", keep, batch)
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) != 30*time.Second {
				t.Errorf("per-run deadline=%v present=%v", deadline, ok)
			}
			mu.Lock()
			calls = append(calls, time.Since(start))
			n := len(calls)
			mu.Unlock()
			if n == 7 {
				return nil
			}
			<-ctx.Done()
			return fmt.Errorf("interrupted prune: %w", ctx.Err())
		}}
		s.scheduleAnalysisGenerationPrune(writer)
		synctest.Wait()
		s.scheduleAnalysisGenerationPrune(writer) // coalesce an in-flight request
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if got := attempts(); !s.analysisPruneScheduled.Load() || len(got) != 1 {
			t.Fatalf("retry slot released early: scheduled=%v calls=%v", s.analysisPruneScheduled.Load(), got)
		}
		s.scheduleAnalysisGenerationPrune(writer) // coalesce during backoff too
		time.Sleep(1200 * time.Second)
		synctest.Wait()
		want := []time.Duration{0, 60 * time.Second, 150 * time.Second, 300 * time.Second, 570 * time.Second, 900 * time.Second, 1230 * time.Second}
		if got := attempts(); !reflect.DeepEqual(got, want) {
			t.Fatalf("attempt starts=%v want=%v", got, want)
		}
		if s.analysisPruneScheduled.Load() {
			t.Fatal("completed prune retained its slot")
		}
	})
}

func TestAnalysisGenerationPruneDrainCancelsRunAndBackoff(t *testing.T) {
	for _, backoff := range []bool{false, true} {
		t.Run(fmt.Sprintf("backoff-%v", backoff), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &Server{}
				t.Cleanup(s.DrainBackground)
				calls := 0
				writer := &analysisPruneTestStore{prune: func(ctx context.Context, _, _ int) error {
					calls++
					<-ctx.Done()
					return ctx.Err()
				}}
				s.scheduleAnalysisGenerationPrune(writer)
				synctest.Wait()
				if backoff {
					time.Sleep(30 * time.Second)
					synctest.Wait()
				}
				beforeDrain := time.Now()
				s.DrainBackground()
				synctest.Wait()
				if time.Now() != beforeDrain || s.analysisPruneScheduled.Load() {
					t.Fatal("drain waited for a deadline or left maintenance running")
				}
				s.scheduleAnalysisGenerationPrune(writer)
				time.Sleep(time.Hour)
				synctest.Wait()
				if calls != 1 || s.analysisPruneScheduled.Load() {
					t.Fatalf("prune escaped drain: calls=%d scheduled=%v", calls, s.analysisPruneScheduled.Load())
				}
			})
		})
	}
}

func TestAnalysisGenerationPruneDoesNotRetryOtherErrors(t *testing.T) {
	for _, failure := range []error{errors.New("storage failure"), context.Canceled, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &Server{}
				t.Cleanup(s.DrainBackground)
				calls := 0
				writer := &analysisPruneTestStore{prune: func(context.Context, int, int) error {
					calls++
					return failure // not an expired per-run context
				}}
				s.scheduleAnalysisGenerationPrune(writer)
				time.Sleep(time.Hour)
				synctest.Wait()
				if calls != 1 || s.analysisPruneScheduled.Load() {
					t.Fatalf("non-retryable failure: calls=%d scheduled=%v", calls, s.analysisPruneScheduled.Load())
				}
			})
		})
	}
}
