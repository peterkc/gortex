package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/semantic/goanalysis"
	"go.uber.org/zap"
)

func TestStartupGoPolicySpansKnownBaseJoinAndReleasesOnEveryExit(t *testing.T) {
	for _, outcome := range []string{"normal", "warmup incomplete", "base failure", "base interruption", "context cancellation"} {
		t.Run(outcome, func(t *testing.T) {
			provider := goanalysis.NewProvider(goanalysis.ModeTypeCheck, false, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, provider.Close()) })
			synctest.Test(t, func(t *testing.T) {
				task := newDaemonWarmup(t.Context())
				releaseJoin := make(chan struct{})
				var active, entered, exited atomic.Bool
				var report knownBasePreparation
				var joinErr error
				// Exercise the real startup worker and existing JoinScheduled completion.
				// daemon.go's source-owned defer follows this same ownership sequence.
				require.True(t, task.start(func(ctx context.Context) {
					release := provider.BeginStartupWarmup()
					active.Store(true)
					defer func() { release(); active.Store(false); exited.Store(true) }()
					if outcome == "warmup incomplete" {
						return
					}
					report, joinErr = joinKnownStartupBases(ctx, startupBaseJoinFunc(func(ctx context.Context) ([]indexer.InitialBasePublication, error) {
						entered.Store(true)
						select {
						case <-releaseJoin:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
						switch outcome {
						case "base failure":
							return []indexer.InitialBasePublication{{RepoPrefix: "fixture", Err: errors.New("publication failed")}}, nil
						case "base interruption":
							return nil, context.DeadlineExceeded
						}
						return nil, nil
					}), nil)
				}))
				synctest.Wait()
				activeAtJoin := active.Load()
				if outcome == "context cancellation" {
					task.cancel()
				} else {
					close(releaseJoin)
				}
				task.wait()
				require.True(t, exited.Load())
				require.False(t, active.Load())
				if outcome != "warmup incomplete" {
					require.True(t, entered.Load())
					require.True(t, activeAtJoin)
				}
				switch outcome {
				case "base failure":
					require.Error(t, joinErr)
					require.Equal(t, 1, report.failed)
				case "base interruption", "context cancellation":
					require.Error(t, joinErr)
					require.True(t, report.interrupted)
				default:
					require.NoError(t, joinErr)
				}
			})
		})
	}
}
