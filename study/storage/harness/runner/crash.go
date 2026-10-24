package runner

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var stages = []string{"before-write", "after-write", "after-sync", "before-rename", "after-rename", "after-dirsync"}

type CrashTrial struct {
	Trial         int          `json:"trial"`
	Kind          string       `json:"kind"`
	Checkpoint    string       `json:"checkpoint"`
	Mode          string       `json:"mode"`
	Allowed       string       `json:"allowed_view"`
	DelayNS       int64        `json:"delay_ns,omitempty"`
	CommitPoint   string       `json:"commit_point"`
	Killed        bool         `json:"killed"`
	OK            bool         `json:"ok"`
	RecoveredView adapter.View `json:"recovered_view,omitempty"`
	Error         string       `json:"error,omitempty"`
}
type publishRequest struct {
	Dir      string
	Set, Old adapter.View
	Single   bool
	External bool
	DelayNS  int64
}

func publishChild(factory adapter.CatalogFactory, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("child requires request path")
	}
	var req publishRequest
	if e := dataset.ReadJSON(args[0], &req); e != nil {
		return e
	}
	cat, e := factory(req.Dir)
	if e != nil {
		return e
	}
	defer cat.Close()
	scan := bufio.NewScanner(os.Stdin)
	if req.External {
		adapter.Checkpoint = func(string) {}
		fmt.Println("GX_READY")
		if !scan.Scan() || scan.Text() != "start" {
			return fmt.Errorf("external crash child requires start")
		}
		from, to := req.Old, req.Set
		for {
			if e = cat.PublishSet(context.Background(), to, from); e != nil {
				return e
			}
			from, to = to, from
		}
	}
	adapter.Checkpoint = func(stage string) {
		fmt.Printf("GX_CHECKPOINT %s\n", stage)
		if !scan.Scan() || scan.Text() != "continue" {
			os.Exit(3)
		}
	}
	if req.Single {
		for repo, ref := range req.Set {
			old := req.Old[repo]
			return cat.Publish(context.Background(), repo, ref, &old)
		}
	}
	return cat.PublishSet(context.Background(), req.Set, req.Old)
}
func allowedAt(stage string) string {
	if stage == "after-rename" || stage == "after-dirsync" {
		return "new"
	}
	return "old"
}
func CheckView(got, old, next adapter.View, validate func(adapter.SegmentRef) error) error {
	return CheckViewAllowed(got, old, next, "either", validate)
}

// Hook trials enforce the side of the commit point. External-time trials allow
// either complete view but never a mixed view. Every reference must open.
func CheckViewAllowed(got, old, next adapter.View, allowed string, validate func(adapter.SegmentRef) error) error {
	valid := false
	switch allowed {
	case "old":
		valid = maps.Equal(got, old)
	case "new":
		valid = maps.Equal(got, next)
	case "either":
		valid = maps.Equal(got, old) || maps.Equal(got, next)
	default:
		return fmt.Errorf("unknown allowed view %q", allowed)
	}
	if !valid {
		return fmt.Errorf("recovered view is mixed, incomplete, unknown or wrong side of commit (%s required)", allowed)
	}
	for repo, ref := range got {
		if ref.Repo != repo || ref.Version == "" || ref.Digest == "" {
			return fmt.Errorf("invalid published reference")
		}
		if e := validate(ref); e != nil {
			return fmt.Errorf("published incomplete reference %s: %w", repo, e)
		}
	}
	return nil
}
func killPublish(ctx context.Context, req publishRequest, requestPath, stage string, safetyTimeout time.Duration) (bool, error) {
	if e := dataset.WriteJSON(requestPath, req); e != nil {
		return false, e
	}
	exe, e := os.Executable()
	if e != nil {
		return false, e
	}
	childCtx, cancel := context.WithTimeout(ctx, safetyTimeout)
	defer cancel()
	cmd := exec.CommandContext(childCtx, exe, "__catalog_publish", requestPath)
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		return false, e
	}
	stdin, e := cmd.StdinPipe()
	if e != nil {
		return false, e
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if e = cmd.Start(); e != nil {
		return false, e
	}
	scan := bufio.NewScanner(stdout)
	killed := false
	stageIndex := 0
	for scan.Scan() {
		line := scan.Text()
		if req.External && line == "GX_READY" {
			if _, e = fmt.Fprintln(stdin, "start"); e != nil {
				break
			}
			timer := time.NewTimer(time.Duration(req.DelayNS))
			select {
			case <-timer.C:
			case <-childCtx.Done():
				timer.Stop()
			}
			e = cmd.Process.Kill()
			killed = e == nil
			break
		}
		if !strings.HasPrefix(line, "GX_CHECKPOINT ") {
			continue
		}
		got := strings.TrimPrefix(line, "GX_CHECKPOINT ")
		if stageIndex >= len(stages) || got != stages[stageIndex] {
			e = fmt.Errorf("checkpoint order violation: index=%d got=%s", stageIndex, got)
			cmd.Process.Kill()
			break
		}
		stageIndex++
		if got == stage {
			e = cmd.Process.Kill()
			killed = e == nil
			break
		}
		if _, e = fmt.Fprintln(stdin, "continue"); e != nil {
			break
		}
	}
	waitErr := cmd.Wait()
	stdin.Close()
	if e != nil {
		return killed, e
	}
	if childCtx.Err() != nil {
		return killed, childCtx.Err()
	}
	if scan.Err() != nil {
		return killed, scan.Err()
	}
	if !killed {
		return false, fmt.Errorf("child did not reach kill point %s: %v %s", stage, waitErr, stderr.String())
	}
	exit, ok := waitErr.(*exec.ExitError)
	if !ok {
		return killed, fmt.Errorf("child did not exit by SIGKILL: %v", waitErr)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		return killed, fmt.Errorf("child termination was not SIGKILL: status=%v wait=%v stderr=%s", status, waitErr, stderr.String())
	}
	return true, nil
}
func recovered(ctx context.Context, b adapter.Backend, cat adapter.Catalog, old, next adapter.View, allowed string) (adapter.View, error) {
	if e := cat.Recover(ctx); e != nil {
		return nil, e
	}
	var repos []string
	for repo := range old {
		repos = append(repos, repo)
	}
	got, release, e := cat.Pin(ctx, repos)
	if e != nil {
		return nil, e
	}
	defer release()
	e = CheckViewAllowed(got, old, next, allowed, func(ref adapter.SegmentRef) error {
		s, e := b.Open(ctx, ref.Path, ref)
		if e != nil {
			return e
		}
		return s.Close()
	})
	return got, e
}
func crashTests(ctx context.Context, b adapter.Backend, factory adapter.CatalogFactory, base dataset.Dataset, oldA, newA adapter.SegmentRef, dir string, trials int, safetyTimeout time.Duration) ([]CrashTrial, error) {
	if e := os.MkdirAll(dir, 0755); e != nil {
		return nil, e
	}
	change, e := MakeChange(base)
	if e != nil {
		return nil, e
	}
	small := change.Replacement
	small.Meta = base.Meta
	peer := dataset.Derive(small, base.Repo+"-crash-peer", 1, 20261022)
	oldB, _, e := b.Build(ctx, peer, filepath.Join(dir, "peer-old"))
	if e != nil {
		return nil, e
	}
	peerChange, e := MakeChange(peer)
	if e != nil {
		return nil, e
	}
	newB, _, e := b.BuildDelta(ctx, oldB, peerChange, filepath.Join(dir, "peer-new"))
	if e != nil {
		return nil, e
	}
	old := adapter.View{oldA.Repo: oldA, oldB.Repo: oldB}
	next := adapter.View{newA.Repo: newA, newB.Repo: newB}
	probe, e := factory(filepath.Join(dir, "completed-set"))
	if e != nil {
		return nil, e
	}
	if probe.CommitPoint() == "" {
		probe.Close()
		return nil, fmt.Errorf("catalog must declare commit point")
	}
	commitPoint := probe.CommitPoint()
	if e = probe.PublishSet(ctx, old, adapter.View{}); e != nil {
		probe.Close()
		return nil, e
	}
	start := time.Now()
	e = probe.PublishSet(ctx, next, old)
	duration := time.Since(start)
	if e == nil {
		_, e = recovered(ctx, b, probe, old, next, "new")
	}
	probe.Close()
	if e != nil {
		return nil, fmt.Errorf("uncrashed PublishSet update failed: %w", e)
	}
	rng := rand.New(rand.NewPCG(20261022, 77))
	perms := [][]int{rng.Perm(6), rng.Perm(6)}
	var result []CrashTrial
	for i := 0; i < trials*2; i++ {
		trialDir := filepath.Join(dir, fmt.Sprintf("trial-%02d", i))
		cat, e := factory(trialDir)
		if e != nil {
			return result, e
		}
		if e = cat.PublishSet(ctx, old, adapter.View{}); e != nil {
			cat.Close()
			return result, e
		}
		cat.Close()
		external := i >= trials
		single := !external && i%2 == 0
		set, expected, allowedNext := next, old, next
		kind := "PublishSet"
		if single {
			set = adapter.View{newA.Repo: newA}
			expected = adapter.View{oldA.Repo: oldA}
			allowedNext = adapter.View{newA.Repo: newA, oldB.Repo: oldB}
			kind = "Publish"
		}
		stage := stages[perms[i%2][(i/2)%6]]
		allowed := allowedAt(stage)
		mode := "checkpoint"
		delay := int64(0)
		if external {
			mode = "external-time"
			stage = "external"
			allowed = "either"
			delay = int64(rng.Uint64N(uint64(max(duration.Nanoseconds()*2, 1000000)))) + 1
		}
		t := CrashTrial{Trial: i, Kind: kind, Checkpoint: stage, Mode: mode, Allowed: allowed, DelayNS: delay, CommitPoint: commitPoint}
		req := publishRequest{Dir: trialDir, Set: set, Old: expected, Single: single, External: external, DelayNS: delay}
		t.Killed, e = killPublish(ctx, req, filepath.Join(dir, fmt.Sprintf("request-%02d.json", i)), stage, safetyTimeout)
		if e == nil {
			cat, e = factory(trialDir)
			if e == nil {
				t.RecoveredView, e = recovered(ctx, b, cat, old, allowedNext, allowed)
				cat.Close()
			}
		}
		t.OK = e == nil
		if e != nil {
			t.Error = e.Error()
		}
		result = append(result, t)
		if e != nil {
			return result, e
		}
	}
	return result, nil
}
