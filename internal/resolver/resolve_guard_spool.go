package resolver

import (
	"bufio"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

// resolveGuardRecord is the compact, pointer-free subset of reindexJob needed
// by the post-pass cross-package guard. One bounded page stays inline; larger
// corpora spill to a temporary file so a pathological pass where every pending
// edge changes still keeps cross-page state out of the Go heap. Live edges are
// recovered later through one batched source-site query per page.
type resolveGuardRecord struct {
	From       string
	CurrentTo  string
	OldTo      string
	Kind       graph.EdgeKind
	FilePath   string
	Line       int
	CrossRepo  bool
	Confidence float64
	Origin     string
	Payload    persistedEdgeSpoolSnapshot
}

type resolveGuardSpool struct {
	scratchParent string // empty retains the native default temporary directory
	records       []resolveGuardRecord
	readAt        int
	bytes         int
	file          *os.File
	writer        *bufio.Writer
	encoder       *gob.Encoder
	decoder       *gob.Decoder
	count         int
}

// A resolveGuardRecord occupies 280 bytes on 64-bit Go before the bytes
// referenced by its strings and Meta slice. Keep additional per-record
// headroom so the inline byte limit remains conservative across allocator
// alignment and small representation changes.
const resolveGuardRecordFixedBytes = 320

func newResolveGuardSpool() (*resolveGuardSpool, error) {
	return &resolveGuardSpool{}, nil
}

func (s *resolveGuardSpool) close() {
	if s == nil {
		return
	}
	s.records = nil
	if s.file != nil {
		if s.writer != nil {
			_ = s.writer.Flush()
		}
		name := s.file.Name()
		_ = s.file.Close()
		_ = os.Remove(name)
		s.file = nil
	}
}

func (s *resolveGuardSpool) appendJobs(groups [][]reindexJob) error {
	for i := range groups {
		for j := range groups[i] {
			job := &groups[i][j]
			if job.edge == nil || !guardCandidateJob(job) {
				continue
			}
			payload := spoolSnapshotPersistedEdge(job.edge)
			record := resolveGuardRecord{
				From: job.edge.From, CurrentTo: job.newTo, OldTo: job.oldTo,
				Kind: job.kind, FilePath: job.edge.FilePath, Line: job.edge.Line,
				CrossRepo: job.crossRepo, Confidence: job.confidence, Origin: job.origin,
				Payload: payload,
			}
			if err := s.append(record); err != nil {
				return err
			}
		}
	}
	return nil
}

func resolveGuardRecordSize(record resolveGuardRecord) int {
	payload := record.Payload
	return resolveGuardRecordFixedBytes + len(record.From) + len(record.CurrentTo) + len(record.OldTo) +
		len(record.Kind) + len(record.FilePath) + len(record.Origin) +
		len(payload.From) + len(payload.To) + len(payload.Kind) + len(payload.FilePath) +
		len(payload.ConfidenceLabel) + len(payload.Origin) + len(payload.Tier) + len(payload.Meta)
}

func (s *resolveGuardSpool) append(record resolveGuardRecord) error {
	recordBytes := resolveGuardRecordSize(record)
	if s.file == nil && len(s.records) < resolvePendingPageRows && s.bytes+recordBytes <= resolveSpoolInlineBytes {
		s.records = append(s.records, record)
		s.bytes += recordBytes
		s.count++
		return nil
	}
	if s.file == nil {
		if err := s.spill(); err != nil {
			return err
		}
	}
	if err := s.encoder.Encode(&record); err != nil {
		return err
	}
	s.count++
	return nil
}

func (s *resolveGuardSpool) spill() error {
	file, err := os.CreateTemp(s.scratchParent, "gortex-resolve-guard-*")
	if err != nil {
		return err
	}
	s.file = file
	s.writer = bufio.NewWriterSize(file, resolveSpoolBufferBytes)
	s.encoder = gob.NewEncoder(s.writer)
	for i := range s.records {
		if err := s.encoder.Encode(&s.records[i]); err != nil {
			return err
		}
	}
	s.records = nil
	s.bytes = 0
	return nil
}

func guardCandidateJob(job *reindexJob) bool {
	if job == nil || !isCallLikeEdge(job.kind) || !isBareNameCallTarget(job.oldTo) {
		return false
	}
	origin := job.origin
	if origin == "" {
		origin = graph.DefaultOriginFor(job.kind, job.confidence, "")
	}
	return origin == graph.OriginTextMatched || origin == graph.OriginASTInferred
}

func (s *resolveGuardSpool) beginRead() error {
	if s.file == nil {
		return nil
	}
	if err := s.writer.Flush(); err != nil {
		return err
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	s.decoder = gob.NewDecoder(bufio.NewReaderSize(s.file, resolveSpoolBufferBytes))
	return nil
}

func (s *resolveGuardSpool) nextPage(limit int) ([]resolveGuardRecord, bool, error) {
	if s.file != nil && s.decoder == nil {
		if err := s.beginRead(); err != nil {
			return nil, false, err
		}
	}
	if limit <= 0 {
		limit = resolvePendingPageRows
	}
	if s.readAt >= s.count {
		return nil, true, nil
	}
	if s.file == nil {
		start := s.readAt
		s.readAt = min(s.readAt+limit, len(s.records))
		return s.records[start:s.readAt], s.readAt == s.count, nil
	}
	remaining := s.count - s.readAt
	targetRows := min(limit, remaining)
	records := make([]resolveGuardRecord, 0, targetRows)
	for len(records) < targetRows {
		var record resolveGuardRecord
		if err := s.decoder.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, false, fmt.Errorf("resolve guard spool ended after %d of %d records: %w", s.readAt+len(records), s.count, io.ErrUnexpectedEOF)
			}
			return nil, false, err
		}
		records = append(records, record)
	}
	s.readAt += len(records)
	return records, s.readAt == s.count, nil
}

func guardJobsFromRecords(store graph.Store, records []resolveGuardRecord) []reindexJob {
	if len(records) == 0 {
		return nil
	}
	sites := make([]graph.EdgeSite, 0, len(records))
	for _, record := range records {
		sites = append(sites, graph.EdgeSite{From: record.From, Line: record.Line, Kind: record.Kind})
	}
	candidates := store.GetEdgeCandidates(nil, sites)
	jobs := make([]reindexJob, 0, len(records))
	for _, record := range records {
		payload := record.Payload.snapshot()
		var live *graph.Edge
		for _, edge := range candidates.Site(record.From, record.Line, record.Kind) {
			if payload.matches(edge) {
				live = edge
				break
			}
		}
		if live == nil {
			continue
		}
		jobs = append(jobs, reindexJob{
			edge: live, oldTo: record.OldTo, newTo: record.CurrentTo,
			kind: record.Kind, crossRepo: record.CrossRepo,
			confidence: record.Confidence, origin: record.Origin, meta: live.Meta,
		})
	}
	return jobs
}

func (r *Resolver) warmGuardLookupCache(jobs []reindexJob) {
	ids := make([]string, 0, len(jobs)*2)
	seen := make(map[string]struct{}, len(jobs)*2)
	for i := range jobs {
		for _, id := range []string{jobs[i].edge.From, jobs[i].newTo} {
			if id == "" {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	r.nodeByID = r.cachedParallelGetNodesByIDs(ids)
	namesByRepo := make(map[string]map[string]struct{})
	for i := range jobs {
		target := r.nodeByID[jobs[i].newTo]
		source := r.nodeByID[jobs[i].edge.From]
		if target == nil || source == nil || target.Name == "" {
			continue
		}
		names := namesByRepo[source.RepoPrefix]
		if names == nil {
			names = make(map[string]struct{})
			namesByRepo[source.RepoPrefix] = names
		}
		names[target.Name] = struct{}{}
		// loneMemberDefnKeep also probes the receiver's declared type through
		// hasInRepoType. Preload that name in the same repository batch so the
		// guard never degrades into one SQLite point lookup per member edge.
		if receiverType := edgeReceiverType(jobs[i].edge); receiverType != "" {
			names[receiverType] = struct{}{}
		}
	}
	repos := make([]string, 0, len(namesByRepo))
	for repo := range namesByRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	scopes := make([]graph.ResolverNameScope, 0, len(repos))
	for _, repo := range repos {
		nameSet := namesByRepo[repo]
		names := make([]string, 0, len(nameSet))
		for name := range nameSet {
			names = append(names, name)
		}
		sort.Strings(names)
		scopes = append(scopes, graph.ResolverNameScope{
			RepoPrefix: repo,
			Names:      names,
		})
	}
	results, err := graph.FindNodesByResolverNameScopes(r.graph, scopes)
	if err != nil {
		r.logger.Warn("resolver: guard name lookup failed", zap.Error(err))
		return
	}
	if len(results) != len(scopes) {
		r.logger.Warn("resolver: guard name lookup returned incomplete scopes",
			zap.Int("results", len(results)),
			zap.Int("scopes", len(scopes)))
		return
	}
	nodesByRepoName := make(map[string]map[string][]*graph.Node, len(repos))
	for i, repo := range repos {
		hits := results[i]
		if hits == nil {
			hits = make(map[string][]*graph.Node, len(scopes[i].Names))
		}
		for _, name := range scopes[i].Names {
			if _, exists := hits[name]; !exists {
				hits[name] = nil
			}
		}
		nodesByRepoName[repo] = hits
	}
	r.nodesByRepoName = nodesByRepoName
}
