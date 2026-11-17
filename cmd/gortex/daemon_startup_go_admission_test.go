package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
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
				var active, entered, exited, closedAtJoin atomic.Bool
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
					release()
					active.Store(false)
					closedAtJoin.Store(true)
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
					require.True(t, closedAtJoin.Load())
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

// The startup owner must release at the join boundary, not at the end of its
// longer tail. Inspect the production closure because a copied callback would
// not catch a release left only in daemon.go's defer.
func TestStartupGoPolicyEndsAtKnownBaseJoinBoundary(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "daemon.go", nil, 0)
	require.NoError(t, err)
	var body *ast.BlockStmt
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name, ok := call.Fun.(*ast.Ident)
		if !ok || name.Name != "joinKnownStartupBases" {
			return true
		}
		// Find the function literal containing the actual startup join.
		ast.Inspect(file, func(candidate ast.Node) bool {
			fn, ok := candidate.(*ast.FuncLit)
			if ok && fn.Pos() < call.Pos() && call.End() < fn.End() {
				body = fn.Body
			}
			return true
		})
		return false
	})
	require.NotNil(t, body)
	joined := false
	released := false
	for _, statement := range body.List {
		ast.Inspect(statement, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, ok := call.Fun.(*ast.Ident)
			if ok && name.Name == "joinKnownStartupBases" {
				joined = true
			}
			if joined && ok && name.Name == "releaseStartupGo" {
				released = true
			}
			return true
		})
		branch, ok := statement.(*ast.IfStmt)
		if !ok {
			continue
		}
		binary, ok := branch.Cond.(*ast.BinaryExpr)
		if !ok {
			continue
		}
		name, ok := binary.X.(*ast.Ident)
		if ok && name.Name == "baseErr" {
			require.True(t, released, "startup policy remains open after the join/log boundary; defer alone releases it after unrelated tail work")
			return
		}
	}
	t.Fatal("production startup join error boundary not found")
}
