package semantic

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

type exportPolicyProvider struct {
	prepare         func(context.Context) error
	bind            func(context.Context) (*EnrichResult, error)
	prepared, bound int
	unavailable     bool
}

func (p *exportPolicyProvider) Name() string                                      { return "export-policy" }
func (p *exportPolicyProvider) Languages() []string                               { return []string{"go"} }
func (p *exportPolicyProvider) Available() bool                                   { return !p.unavailable }
func (p *exportPolicyProvider) Close() error                                      { return nil }
func (p *exportPolicyProvider) Enrich(graph.Store, string) (*EnrichResult, error) { return nil, nil }
func (p *exportPolicyProvider) EnrichFile(graph.Store, string, string) (*EnrichResult, error) {
	return nil, nil
}
func (p *exportPolicyProvider) PrepareFileExportsContext(ctx context.Context, _, _ string, _ []string) error {
	p.prepared++
	if p.prepare != nil {
		return p.prepare(ctx)
	}
	return nil
}
func (p *exportPolicyProvider) EnrichFilesContext(ctx context.Context, _ graph.Store, _, _ string, _ []string) (*EnrichResult, error) {
	p.bound++
	if p.bind != nil {
		return p.bind(ctx)
	}
	return &EnrichResult{}, nil
}

type exportPolicyNoCapability struct {
	Provider
	batch *exportPolicyProvider
}

func (p *exportPolicyNoCapability) EnrichFilesContext(ctx context.Context, g graph.Store, prefix, root string, files []string) (*EnrichResult, error) {
	return p.batch.EnrichFilesContext(ctx, g, prefix, root, files)
}

type exportPolicySupplemental struct{ *exportPolicyProvider }

func (*exportPolicySupplemental) Supplemental() bool { return true }
func (*exportPolicySupplemental) Name() string       { return "supplemental-export-policy" }

func TestStartupFileExportPolicy(t *testing.T) {
	t.Run("ordinary provider owns late result", func(t *testing.T) {
		for _, partial := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				p := &exportPolicyProvider{bind: func(ctx context.Context) (*EnrichResult, error) {
					<-ctx.Done()
					return &EnrichResult{Partial: partial}, nil
				}}
				m := NewManager(Config{Enabled: true, TimeoutSeconds: 1}, zap.NewNop())
				defer m.Close()
				m.RegisterProvider(p)
				result, err := m.EnrichFiles(graph.New(), "r", t.TempDir(), "go", []string{"r/a.go"})
				require.NoError(t, err)
				require.Equal(t, partial, result.Partial)
			})
		}
	})
	t.Run("no capability and supplemental", func(t *testing.T) {
		primary := &exportPolicyProvider{}
		extra := &exportPolicyProvider{}
		m := NewManager(Config{Enabled: true, TimeoutSeconds: 2}, zap.NewNop())
		defer m.Close()
		m.RegisterProvider(&exportPolicyNoCapability{Provider: primary, batch: primary})
		m.RegisterProvider(&exportPolicySupplemental{extra})
		_, err := m.EnrichStartupFilesContext(t.Context(), graph.New(), "r", t.TempDir(), "go", []string{"a.go"})
		require.NoError(t, err)
		require.Equal(t, 1, primary.bound)
		require.Equal(t, 1, extra.bound)
		require.Zero(t, primary.prepared)
		require.Zero(t, extra.prepared)
	})
	require.Equal(t, 480*time.Second, startupFileExportTimeout)
	require.Equal(t, 120, DefaultConfig().TimeoutSeconds)
	t.Run("fresh partial deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := &exportPolicyProvider{}
			p.prepare = func(ctx context.Context) error {
				d, ok := ctx.Deadline()
				require.True(t, ok)
				require.Equal(t, 480*time.Second, time.Until(d))
				<-time.After(3 * time.Second)
				p.unavailable = true
				return nil
			}
			p.bind = func(ctx context.Context) (*EnrichResult, error) {
				d, ok := ctx.Deadline()
				require.True(t, ok)
				require.Equal(t, 2*time.Second, time.Until(d))
				return &EnrichResult{}, nil
			}
			m := NewManager(Config{Enabled: true, TimeoutSeconds: 2}, zap.NewNop())
			m.RegisterProvider(p)
			defer m.Close()
			_, err := m.EnrichStartupFilesContext(t.Context(), graph.New(), "r", t.TempDir(), "go", []string{"r/a.go"})
			require.NoError(t, err)
			require.Equal(t, 1, p.bound)
		})
	})
	for _, mode := range []string{"failure", "deadline", "cancel racing nil", "close"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				entered := make(chan struct{})
				p := &exportPolicyProvider{}
				p.prepare = func(ctx context.Context) error {
					close(entered)
					switch mode {
					case "failure":
						return errors.New("export failed")
					case "cancel racing nil":
						cancel()
						return nil
					default:
						<-ctx.Done()
						return ctx.Err()
					}
				}
				m := NewManager(Config{Enabled: true, TimeoutSeconds: 2}, zap.NewNop())
				m.RegisterProvider(p)
				defer m.Close()
				done := make(chan error, 1)
				go func() {
					_, err := m.EnrichStartupFilesContext(ctx, graph.New(), "r", t.TempDir(), "go", []string{"r/a.go"})
					done <- err
				}()
				<-entered
				if mode == "close" {
					require.NoError(t, m.Close())
				}
				err := <-done
				require.Error(t, err)
				require.Zero(t, p.bound)
				if mode == "deadline" {
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.NoError(t, ctx.Err(), "local deadline must not cancel siblings")
				}
			})
		})
	}
	t.Run("ordinary empty disabled and no capability", func(t *testing.T) {
		p := &exportPolicyProvider{}
		m := NewManager(Config{Enabled: true, TimeoutSeconds: 2}, zap.NewNop())
		m.RegisterProvider(p)
		defer m.Close()
		_, err := m.EnrichFiles(graph.New(), "r", t.TempDir(), "go", []string{"a.go"})
		require.NoError(t, err)
		require.Zero(t, p.prepared)
		require.Equal(t, 1, p.bound)
		_, err = m.EnrichStartupFilesContext(t.Context(), graph.New(), "r", t.TempDir(), "go", nil)
		require.NoError(t, err)
		require.Zero(t, p.prepared)
		m.config.Enabled = false
		_, err = m.EnrichStartupFilesContext(t.Context(), graph.New(), "r", t.TempDir(), "go", []string{"a.go"})
		require.NoError(t, err)
		require.Zero(t, p.prepared)
	})
}
