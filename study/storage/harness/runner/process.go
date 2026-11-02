package runner

import (
	"context"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"net"
	"net/rpc"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// ProcessRequest is the study-only process boundary. Dataset values carry disk
// descriptors in real runs, never a serialized copy of the source corpus.
type ProcessRequest struct {
	Op            string
	Data          []dataset.Dataset
	Dataset       dataset.Dataset
	Change        adapter.FileChange
	Path          string
	Ref           adapter.SegmentRef
	OldRef        *adapter.SegmentRef
	Handle, Token int
	Repos         []string
	View, Old     adapter.View
	Handles       map[string]int
	Query         workload.Query
	Answer        workload.Answer
	Inputs        []adapter.SearchInput
	Text          string
	Deadline      int64
}
type ProcessReply struct {
	Error, Code         string
	Name                string
	Capabilities        adapter.Capabilities
	NativeMulti, Server bool
	Handle, Token       int
	Ref                 adapter.SegmentRef
	Stats               adapter.BuildStats
	Answer              workload.Answer
	Inputs              []adapter.SearchInput
	View                adapter.View
	Usage               adapter.ResourceUsage
	Check               workload.Check
	Elapsed             int64
	Start, End          time.Time
	Publication         publicationTimes
	Metrics             map[string]int64
	Recovery            catalogReadResult
	Footprint           map[string]int64
}
type ProcessService struct {
	backend       adapter.Backend
	factory       adapter.CatalogFactory
	checker       bool
	mu            sync.Mutex
	next          int
	segments      map[int]adapter.Segment
	catalogs      map[int]adapter.Catalog
	multis        map[int]adapter.MultiRepo
	releases      map[int]func()
	cleanupErrors []error
}

func (s *ProcessService) putSegment(v adapter.Segment) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	s.segments[s.next] = v
	return s.next
}
func (s *ProcessService) Call(req ProcessRequest, out *ProcessReply) (callErr error) {
	ctx := context.Background()
	if req.Deadline != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.Unix(0, req.Deadline))
		defer cancel()
	}
	defer func() {
		if callErr != nil {
			out.Error = callErr.Error()
			if errors.Is(callErr, adapter.ErrNotFound) {
				out.Code = "not-found"
			}
			if errors.Is(callErr, adapter.ErrUnsupported) {
				out.Code = "unsupported"
			}
			if errors.Is(callErr, adapter.ErrConflict) {
				out.Code = "conflict"
			}
			callErr = nil
		}
	}()
	if s.checker && req.Op != "check" && req.Op != "describe" {
		return fmt.Errorf("checker refuses engine operation")
	}
	s.mu.Lock()
	segment := s.segments[req.Handle]
	catalog := s.catalogs[req.Handle]
	multi := s.multis[req.Handle]
	s.mu.Unlock()
	switch req.Op {
	case "describe":
		out.Name = s.backend.Name()
		out.Capabilities = s.backend.Capabilities()
		_, out.NativeMulti = s.backend.(adapter.NativeMulti)
		_, out.Server = s.backend.(adapter.ServerHooks)
	case "build":
		start := time.Now()
		ref, stats, err := s.backend.Build(ctx, req.Dataset, req.Path)
		out.End = time.Now()
		out.Start = start
		out.Elapsed = out.End.Sub(start).Nanoseconds()
		out.Ref = ref
		out.Stats = stats
		return err
	case "delta":
		start := time.Now()
		ref, stats, err := s.backend.BuildDelta(ctx, req.Ref, req.Change, req.Path)
		out.End = time.Now()
		out.Start = start
		out.Elapsed = out.End.Sub(start).Nanoseconds()
		out.Ref = ref
		out.Stats = stats
		return err
	case "open":
		start := time.Now()
		v, err := s.backend.Open(ctx, req.Path, req.Ref)
		out.Start, out.End = start, time.Now()
		out.Elapsed = out.End.Sub(out.Start).Nanoseconds()
		if err != nil {
			return err
		}
		out.Handle = s.putSegment(v)
	case "close":
		if segment == nil {
			return fmt.Errorf("unknown segment handle")
		}
		err := segment.Close()
		s.mu.Lock()
		if err != nil {
			s.cleanupErrors = append(s.cleanupErrors, err)
		}
		delete(s.segments, req.Handle)
		s.mu.Unlock()
		return err
	case "query":
		segments := map[string]adapter.Segment{}
		s.mu.Lock()
		for repo, id := range req.Handles {
			v := s.segments[id]
			if v == nil {
				s.mu.Unlock()
				return fmt.Errorf("unknown query segment")
			}
			segments[repo] = v
		}
		s.mu.Unlock()
		if multi == nil {
			multi = adapter.Fanout{Segments: segments}
		}
		multi, inputs := traceNativeFanout(multi, req.Query)
		out.Start = time.Now()
		answer, err := workload.Evaluate(ctx, segments, multi, req.Query)
		out.End = time.Now()
		out.Elapsed = out.End.Sub(out.Start).Nanoseconds()
		out.Answer = answer
		out.Inputs = inputs
		return err
	case "segment-query":
		if segment == nil {
			return fmt.Errorf("unknown segment")
		}
		q := req.Query
		q.Repo = "segment"
		answer, err := workload.Evaluate(ctx, map[string]adapter.Segment{"segment": segment}, nil, q)
		out.Answer = answer
		return err
	case "multi-open":
		native, ok := s.backend.(adapter.NativeMulti)
		if !ok {
			return adapter.ErrUnsupported
		}
		segments := map[string]adapter.Segment{}
		s.mu.Lock()
		for repo, id := range req.Handles {
			segments[repo] = s.segments[id]
		}
		s.mu.Unlock()
		v, err := native.OpenMulti(ctx, segments)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.next++
		out.Handle = s.next
		s.multis[out.Handle] = v
		s.mu.Unlock()
	case "catalog-open":
		v, err := s.factory(req.Path)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.next++
		out.Handle = s.next
		s.catalogs[out.Handle] = v
		s.mu.Unlock()
		out.Name = v.CommitPoint()
	case "publish":
		if catalog == nil {
			return fmt.Errorf("unknown catalog")
		}
		return catalog.Publish(ctx, req.Ref.Repo, req.Ref, req.OldRef)
	case "publish-set":
		if catalog == nil {
			return fmt.Errorf("unknown catalog")
		}
		return catalog.PublishSet(ctx, req.View, req.Old)
	case "verify":
		if catalog == nil {
			return fmt.Errorf("unknown catalog")
		}
		return verifyPublication(ctx, s.backend, catalog, req.Ref)
	case "publish-measured":
		if catalog == nil || req.OldRef == nil {
			return fmt.Errorf("invalid measured publication")
		}
		t, err := measurePublication(ctx, s.backend, catalog, req.Ref, *req.OldRef)
		out.Publication = t
		return err
	case "pin":
		if catalog == nil {
			return fmt.Errorf("unknown catalog")
		}
		out.Start = time.Now()
		view, release, err := catalog.Pin(ctx, req.Repos)
		out.End = time.Now()
		if err != nil {
			return err
		}
		out.View = view
		s.mu.Lock()
		s.next++
		out.Token = s.next
		s.releases[out.Token] = release
		s.mu.Unlock()
	case "release-pin":
		s.mu.Lock()
		release := s.releases[req.Token]
		delete(s.releases, req.Token)
		s.mu.Unlock()
		if release != nil {
			release()
		}
	case "previous":
		if catalog == nil {
			return fmt.Errorf("unknown catalog")
		}
		v, err := catalog.Previous(ctx, req.Text)
		out.Ref = v
		return err
	case "recover":
		if catalog == nil {
			return fmt.Errorf("unknown catalog")
		}
		return catalog.Recover(ctx)
	case "catalog-close":
		if catalog == nil {
			return fmt.Errorf("unknown catalog")
		}
		err := catalog.Close()
		s.mu.Lock()
		if err != nil {
			s.cleanupErrors = append(s.cleanupErrors, err)
		}
		delete(s.catalogs, req.Handle)
		s.mu.Unlock()
		return err
	case "reset", "usage", "release":
		h, ok := s.backend.(adapter.ServerHooks)
		if !ok {
			return adapter.ErrUnsupported
		}
		switch req.Op {
		case "reset":
			return h.Reset(ctx)
		case "release":
			return h.Release(ctx, req.Ref)
		case "usage":
			v, err := h.ResourceUsage(ctx)
			out.Usage = v
			return err
		}
	case "catalog-trial":
		samples, recovery, footprint, err := catalogTrial(s.backend.Name(), s.factory, req.Old, req.View, req.Repos, req.Path)
		out.Metrics = samples
		out.Recovery = recovery
		out.Footprint = footprint
		return err
	case "check":
		ids := []string{}
		for _, h := range req.Answer.Hits {
			ids = append(ids, h.ID)
		}
		for _, input := range req.Inputs {
			for _, h := range input.Hits {
				ids = append(ids, h.ID)
			}
		}
		expected, err := oracle.DiskReference(ctx, req.Data, req.Query, ids)
		if err != nil {
			return err
		}
		out.Check = compareRankedAnswer(req.Query, expected, req.Answer, req.Inputs)
	default:
		return fmt.Errorf("unknown process operation %q", req.Op)
	}
	return nil
}

type HookService struct{}

func (HookService) Call(req ProcessRequest, out *ProcessReply) error {
	if req.Op != "hook" {
		return fmt.Errorf("unknown callback")
	}
	adapter.Checkpoint(req.Text)
	return nil
}
func processServe(b adapter.Backend, factory adapter.CatalogFactory, checker bool) error {
	file := os.NewFile(3, "engine-rpc")
	conn, err := net.FileConn(file)
	file.Close()
	if err != nil {
		return err
	}
	defer conn.Close()
	callbackFile := os.NewFile(4, "hook-rpc")
	callbackConn, err := net.FileConn(callbackFile)
	callbackFile.Close()
	if err != nil {
		return err
	}
	defer callbackConn.Close()
	callback := rpc.NewClient(callbackConn)
	defer callback.Close()
	s := &ProcessService{backend: b, factory: factory, checker: checker, segments: map[int]adapter.Segment{}, catalogs: map[int]adapter.Catalog{}, multis: map[int]adapter.MultiRepo{}, releases: map[int]func(){}}
	previous := adapter.Checkpoint
	adapter.Checkpoint = func(stage string) {
		var reply ProcessReply
		if err := callback.Call("Hook.Call", ProcessRequest{Op: "hook", Text: stage}, &reply); err != nil {
			panic(err)
		}
	}
	defer func() { adapter.Checkpoint = previous }()
	server := rpc.NewServer()
	if err = server.RegisterName("Process", s); err != nil {
		return err
	}
	server.ServeConn(conn)
	cleanup := s.cleanupErrors
	for _, release := range s.releases {
		release()
	}
	for _, segment := range s.segments {
		cleanup = append(cleanup, segment.Close())
	}
	for _, cat := range s.catalogs {
		cleanup = append(cleanup, cat.Close())
	}
	return errors.Join(cleanup...)
}

type processClient struct {
	rpc                  *rpc.Client
	cmd                  *exec.Cmd
	hook                 net.Conn
	hookDone             <-chan struct{}
	failure              error
	log                  *os.File
	lastBuild            int64
	buildStart, buildEnd time.Time
	openNS               int64
	mu                   sync.Mutex
}

func socketPair() (net.Conn, *os.File, error) {
	// Mark both originals close-on-exec before another goroutine can fork.
	// ExtraFiles explicitly duplicates the intended child descriptors.
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, err
	}
	parent := os.NewFile(uintptr(fds[0]), "rpc-parent")
	child := os.NewFile(uintptr(fds[1]), "rpc-child")
	conn, err := net.FileConn(parent)
	parent.Close()
	if err != nil {
		child.Close()
		return nil, nil, err
	}
	return conn, child, nil
}
func startProcess(ctx context.Context, scope, path string, checker bool) (client *processClient, err error) {
	conn, child, err := socketPair()
	if err != nil {
		return nil, err
	}
	defer func() {
		if child != nil {
			child.Close()
		}
	}()
	hooks, hookChild, err := socketPair()
	if err != nil {
		conn.Close()
		return nil, err
	}
	defer func() {
		if hookChild != nil {
			hookChild.Close()
		}
	}()
	log, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		conn.Close()
		hooks.Close()
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		conn.Close()
		hooks.Close()
		log.Close()
		return nil, err
	}
	mode := "__engine"
	if checker {
		mode = "__checker"
	}
	cmd := exec.CommandContext(ctx, exe, mode)
	cmd.ExtraFiles = []*os.File{child, hookChild}
	cmd.Stdout = log
	cmd.Stderr = log
	release, err := limitCommand(cmd, scope)
	if err != nil {
		conn.Close()
		hooks.Close()
		log.Close()
		return nil, err
	}
	defer release()
	if err = cmd.Start(); err != nil {
		conn.Close()
		hooks.Close()
		log.Close()
		return nil, err
	}
	// The parent must not retain the peer endpoints while waiting for an RPC.
	// Otherwise an early child exit cannot produce EOF on conn or hooks.
	err = errors.Join(child.Close(), hookChild.Close())
	child, hookChild = nil, nil
	if err != nil {
		killErr := cmd.Process.Kill()
		waitErr := cmd.Wait()
		conn.Close()
		hooks.Close()
		log.Close()
		return nil, errors.Join(err, killErr, waitErr)
	}
	if err = os.WriteFile(path+".pid", []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		conn.Close()
		hooks.Close()
		log.Close()
		return nil, err
	}
	server := rpc.NewServer()
	if err = server.RegisterName("Hook", HookService{}); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		conn.Close()
		hooks.Close()
		log.Close()
		return nil, err
	}
	hookDone := make(chan struct{})
	go func() { defer close(hookDone); server.ServeConn(hooks) }()
	client = &processClient{rpc: rpc.NewClient(conn), cmd: cmd, hook: hooks, hookDone: hookDone, log: log}
	if _, err = client.call(ctx, ProcessRequest{Op: "describe"}); err != nil {
		return nil, errors.Join(err, client.close())
	}
	return client, nil
}
func (c *processClient) call(ctx context.Context, req ProcessRequest) (ProcessReply, error) {
	var out ProcessReply
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		req.Deadline = deadline.UnixNano()
	}
	done := make(chan *rpc.Call, 1)
	call := c.rpc.Go("Process.Call", req, &out, done)
	select {
	case <-ctx.Done():
		c.rpc.Close()
		return out, ctx.Err()
	case <-call.Done:
		if call.Error != nil {
			return out, call.Error
		}
	}
	if out.Error != "" {
		switch out.Code {
		case "not-found":
			return out, fmt.Errorf("%s: %w", out.Error, adapter.ErrNotFound)
		case "unsupported":
			return out, fmt.Errorf("%s: %w", out.Error, adapter.ErrUnsupported)
		case "conflict":
			return out, fmt.Errorf("%s: %w", out.Error, adapter.ErrConflict)
		}
		return out, errors.New(out.Error)
	}
	return out, nil
}
func (c *processClient) close() error {
	c.rpc.Close()
	// EOF joins the owned server; cancellation also terminates native calls.
	wait := make(chan error, 1)
	go func() { wait <- c.cmd.Wait() }()
	timer := time.NewTimer(defaultSafetyTimeout)
	defer timer.Stop()
	var err error
	select {
	case err = <-wait:
	case <-timer.C:
		killErr := c.cmd.Process.Kill()
		err = errors.Join(fmt.Errorf("process cleanup deadline exceeded"), killErr, <-wait)
	}
	hookErr := c.hook.Close()
	if errors.Is(hookErr, net.ErrClosed) {
		hookErr = nil
	}
	<-c.hookDone
	c.mu.Lock()
	failure := c.failure
	c.mu.Unlock()
	return errors.Join(err, hookErr, c.log.Close(), failure)
}

type remoteBackend struct {
	client *processClient
	name   string
	caps   adapter.Capabilities
}

func (b *remoteBackend) Name() string                       { return b.name }
func (b *remoteBackend) Capabilities() adapter.Capabilities { return b.caps }
func (b *remoteBackend) Build(ctx context.Context, d dataset.Dataset, path string) (adapter.SegmentRef, adapter.BuildStats, error) {
	r, e := b.client.call(ctx, ProcessRequest{Op: "build", Dataset: d, Path: path})
	b.client.mu.Lock()
	b.client.lastBuild = r.Elapsed
	b.client.buildStart, b.client.buildEnd = r.Start, r.End
	b.client.mu.Unlock()
	return r.Ref, r.Stats, e
}
func (b *remoteBackend) BuildDelta(ctx context.Context, ref adapter.SegmentRef, c adapter.FileChange, path string) (adapter.SegmentRef, adapter.BuildStats, error) {
	r, e := b.client.call(ctx, ProcessRequest{Op: "delta", Ref: ref, Change: c, Path: path})
	b.client.mu.Lock()
	b.client.lastBuild = r.Elapsed
	b.client.buildStart, b.client.buildEnd = r.Start, r.End
	b.client.mu.Unlock()
	return r.Ref, r.Stats, e
}
func (b *remoteBackend) Open(ctx context.Context, path string, ref adapter.SegmentRef) (adapter.Segment, error) {
	r, e := b.client.call(ctx, ProcessRequest{Op: "open", Path: path, Ref: ref})
	if e != nil {
		return nil, e
	}
	b.client.mu.Lock()
	b.client.openNS += r.Elapsed
	b.client.mu.Unlock()
	return &remoteSegment{client: b.client, id: r.Handle, repo: ref.Repo, openStart: r.Start, openEnd: r.End}, nil
}

type remoteNative struct{ *remoteBackend }

func (b *remoteNative) OpenMulti(ctx context.Context, segments map[string]adapter.Segment) (adapter.MultiRepo, error) {
	r, e := b.client.call(ctx, ProcessRequest{Op: "multi-open", Handles: remoteHandles(segments)})
	if e != nil {
		return nil, e
	}
	return &remoteMulti{client: b.client, id: r.Handle, handles: remoteHandles(segments)}, nil
}

type remoteHooks struct{ *remoteBackend }

func (b *remoteHooks) Reset(ctx context.Context) error {
	_, e := b.client.call(ctx, ProcessRequest{Op: "reset"})
	return e
}
func (b *remoteHooks) Release(ctx context.Context, ref adapter.SegmentRef) error {
	_, e := b.client.call(ctx, ProcessRequest{Op: "release", Ref: ref})
	return e
}
func (b *remoteHooks) ResourceUsage(ctx context.Context) (adapter.ResourceUsage, error) {
	r, e := b.client.call(ctx, ProcessRequest{Op: "usage"})
	return r.Usage, e
}

type remoteBoth struct {
	*remoteNative
	hooks *remoteHooks
}

func (b *remoteBoth) Reset(ctx context.Context) error { return b.hooks.Reset(ctx) }
func (b *remoteBoth) Release(ctx context.Context, ref adapter.SegmentRef) error {
	return b.hooks.Release(ctx, ref)
}
func (b *remoteBoth) ResourceUsage(ctx context.Context) (adapter.ResourceUsage, error) {
	return b.hooks.ResourceUsage(ctx)
}
func remoteCandidate(ctx context.Context, c *processClient) (adapter.Backend, adapter.CatalogFactory, error) {
	r, e := c.call(ctx, ProcessRequest{Op: "describe"})
	if e != nil {
		return nil, nil, e
	}
	base := &remoteBackend{client: c, name: r.Name, caps: r.Capabilities}
	var b adapter.Backend = base
	if r.NativeMulti && r.Server {
		b = &remoteBoth{remoteNative: &remoteNative{base}, hooks: &remoteHooks{base}}
	} else if r.NativeMulti {
		b = &remoteNative{base}
	} else if r.Server {
		b = &remoteHooks{base}
	}
	factory := func(path string) (adapter.Catalog, error) {
		r, e := c.call(context.Background(), ProcessRequest{Op: "catalog-open", Path: path})
		if e != nil {
			return nil, e
		}
		return &remoteCatalog{client: c, id: r.Handle, point: r.Name}, nil
	}
	return b, factory, nil
}

type remoteSegment struct {
	client             *processClient
	id                 int
	repo               string
	openStart, openEnd time.Time
}

func (s *remoteSegment) query(ctx context.Context, q workload.Query) (workload.Answer, error) {
	r, e := s.client.call(ctx, ProcessRequest{Op: "segment-query", Handle: s.id, Query: q})
	return r.Answer, e
}
func (s *remoteSegment) NodeByID(ctx context.Context, id string) (dataset.Node, error) {
	a, e := s.query(ctx, workload.Query{Op: "id", Key: id})
	if e == nil && a.Missing {
		e = adapter.ErrNotFound
	}
	if a.Node == nil {
		return dataset.Node{}, e
	}
	return *a.Node, e
}
func (s *remoteSegment) NodesByName(ctx context.Context, key string, k int) ([]dataset.Node, error) {
	a, e := s.query(ctx, workload.Query{Op: "name", Key: key, K: k})
	return a.Nodes, e
}
func (s *remoteSegment) FileNodes(ctx context.Context, key string) ([]dataset.Node, error) {
	a, e := s.query(ctx, workload.Query{Op: "file", Key: key})
	return a.Nodes, e
}
func (s *remoteSegment) OutEdges(ctx context.Context, key, kind string) ([]dataset.Edge, error) {
	a, e := s.query(ctx, workload.Query{Op: "out", Key: key, Kind: kind})
	return a.Edges, e
}
func (s *remoteSegment) InEdges(ctx context.Context, key, kind string) ([]dataset.Edge, error) {
	a, e := s.query(ctx, workload.Query{Op: "in", Key: key, Kind: kind})
	return a.Edges, e
}
func (s *remoteSegment) SearchSymbols(ctx context.Context, key string, k int) ([]adapter.Hit, error) {
	a, e := s.query(ctx, workload.Query{Op: "symbols", Key: key, K: k})
	return a.Hits, e
}
func (s *remoteSegment) SearchContent(ctx context.Context, key string, k int) ([]adapter.Hit, error) {
	a, e := s.query(ctx, workload.Query{Op: "content", Key: key, K: k})
	return a.Hits, e
}
func (s *remoteSegment) VectorTopK(ctx context.Context, q []float32, k int) ([]adapter.Hit, error) {
	a, e := s.query(ctx, workload.Query{Op: "vector", Vec: q, K: k})
	return a.Hits, e
}
func (s *remoteSegment) Close() error {
	_, e := s.client.call(context.Background(), ProcessRequest{Op: "close", Handle: s.id})
	return e
}
func remoteHandles(segments map[string]adapter.Segment) map[string]int {
	handles := map[string]int{}
	for repo, s := range segments {
		if v, ok := s.(*remoteSegment); ok {
			handles[repo] = v.id
		}
	}
	return handles
}

type remoteMulti struct {
	client  *processClient
	id      int
	handles map[string]int
}

func (m *remoteMulti) query(ctx context.Context, q workload.Query) (workload.Answer, error) {
	r, e := m.client.call(ctx, ProcessRequest{Op: "query", Handle: m.id, Handles: m.handles, Query: q})
	return r.Answer, e
}
func (m *remoteMulti) NodesByName(ctx context.Context, key string, k int) ([]dataset.Node, error) {
	a, e := m.query(ctx, workload.Query{Op: "multi_name", Key: key, K: k})
	return a.Nodes, e
}
func (m *remoteMulti) InEdges(ctx context.Context, key, kind string) ([]dataset.Edge, error) {
	a, e := m.query(ctx, workload.Query{Op: "multi_in", Key: key, Kind: kind})
	return a.Edges, e
}
func (m *remoteMulti) SearchSymbols(ctx context.Context, key string, k int) ([]adapter.Hit, error) {
	a, e := m.query(ctx, workload.Query{Op: "multi_symbols", Key: key, K: k})
	return a.Hits, e
}
func (m *remoteMulti) VectorTopK(ctx context.Context, q []float32, k int) ([]adapter.Hit, error) {
	a, e := m.query(ctx, workload.Query{Op: "multi_vector", Vec: q, K: k})
	return a.Hits, e
}
func (m *remoteMulti) NativeTraverse(ctx context.Context, key string, depth, fanout, visits int) ([]string, error) {
	if fanout != workload.FanoutCap || visits != workload.VisitCap {
		return nil, fmt.Errorf("unsupported traversal caps")
	}
	a, e := m.query(ctx, workload.Query{Op: fmt.Sprintf("native_multi_traverse%d", depth), Key: key, Depth: depth})
	return a.IDs, e
}

type remoteCatalog struct {
	client *processClient
	id     int
	point  string
}

func (c *remoteCatalog) CommitPoint() string { return c.point }
func (c *remoteCatalog) Publish(ctx context.Context, repo string, ref adapter.SegmentRef, old *adapter.SegmentRef) error {
	if ref.Repo != repo {
		return fmt.Errorf("publication repository mismatch")
	}
	_, e := c.client.call(ctx, ProcessRequest{Op: "publish", Handle: c.id, Ref: ref, OldRef: old})
	return e
}
func (c *remoteCatalog) PublishSet(ctx context.Context, set, old adapter.View) error {
	_, e := c.client.call(ctx, ProcessRequest{Op: "publish-set", Handle: c.id, View: set, Old: old})
	return e
}
func (c *remoteCatalog) Pin(ctx context.Context, repos []string) (adapter.View, func(), error) {
	view, release, _, _, err := c.pinTimed(ctx, repos)
	return view, release, err
}
func (c *remoteCatalog) pinTimed(ctx context.Context, repos []string) (adapter.View, func(), time.Time, time.Time, error) {
	r, e := c.client.call(ctx, ProcessRequest{Op: "pin", Handle: c.id, Repos: repos})
	var once sync.Once
	release := func() {
		once.Do(func() {
			_, err := c.client.call(context.Background(), ProcessRequest{Op: "release-pin", Token: r.Token})
			if err != nil {
				c.client.mu.Lock()
				c.client.failure = errors.Join(c.client.failure, err)
				c.client.mu.Unlock()
				c.client.rpc.Close()
			}
		})
	}
	return r.View, release, r.Start, r.End, e
}
func pinTimed(ctx context.Context, cat adapter.Catalog, repos []string) (adapter.View, func(), time.Time, time.Time, error) {
	if remote, ok := cat.(*remoteCatalog); ok {
		return remote.pinTimed(ctx, repos)
	}
	start := time.Now()
	view, release, err := cat.Pin(ctx, repos)
	return view, release, start, time.Now(), err
}
func (c *remoteCatalog) Previous(ctx context.Context, repo string) (adapter.SegmentRef, error) {
	r, e := c.client.call(ctx, ProcessRequest{Op: "previous", Handle: c.id, Text: repo})
	return r.Ref, e
}
func (c *remoteCatalog) Recover(ctx context.Context) error {
	_, e := c.client.call(ctx, ProcessRequest{Op: "recover", Handle: c.id})
	return e
}
func (c *remoteCatalog) Close() error {
	_, e := c.client.call(context.Background(), ProcessRequest{Op: "catalog-close", Handle: c.id})
	return e
}
func (c *remoteCatalog) VerifyPublication(ctx context.Context, ref adapter.SegmentRef) error {
	_, e := c.client.call(ctx, ProcessRequest{Op: "verify", Handle: c.id, Ref: ref})
	return e
}

// evaluateTimed keeps IPC and reference checking outside operation timings.
func evaluateTimed(ctx context.Context, segments map[string]adapter.Segment, m adapter.MultiRepo, q workload.Query) (workload.Answer, []adapter.SearchInput, time.Time, time.Time, error) {
	var client *processClient
	for _, s := range segments {
		if v, ok := s.(*remoteSegment); ok {
			client = v.client
			break
		}
	}
	if client != nil {
		req := ProcessRequest{Op: "query", Handles: remoteHandles(segments), Query: q}
		if native, ok := m.(*remoteMulti); ok {
			req.Handle = native.id
		}
		r, e := client.call(ctx, req)
		return r.Answer, r.Inputs, r.Start, r.End, e
	}
	multi, inputs := traceNativeFanout(m, q)
	start := time.Now()
	a, e := workload.Evaluate(ctx, segments, multi, q)
	end := time.Now()
	return a, inputs, start, end, e
}

func backendClient(b adapter.Backend) *processClient {
	switch v := b.(type) {
	case *trackedBackend:
		return backendClient(v.Backend)
	case *remoteBackend:
		return v.client
	case *remoteNative:
		return v.client
	case *remoteHooks:
		return v.client
	case *remoteBoth:
		return v.remoteNative.client
	}
	return nil
}

type engineScopeKey struct{}
