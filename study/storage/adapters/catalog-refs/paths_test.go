package main

import (
	"context"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRefsPublicationPaths(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	b := backend{}
	d := conformance.Fixture()
	a, _, err := b.Build(ctx, d, filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	change := conformance.Fixture()
	change.Nodes[0].Name = "new Alpha"
	na, _, err := b.Build(ctx, change, filepath.Join(dir, "na"))
	if err != nil {
		t.Fatal(err)
	}
	d.Repo = "peer"
	peer, _, err := b.Build(ctx, d, filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	catDir := filepath.Join(dir, "catalog")
	cat, err := factory(catDir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err = cat.Publish(ctx, a.Repo, a, nil); err != nil {
		t.Fatal(err)
	}
	if err = cat.Publish(ctx, peer.Repo, peer, nil); err != nil {
		t.Fatal(err)
	}
	peerPath := filepath.Join(catDir, "refs", refName(peer.Repo))
	peerBefore, err := os.ReadFile(peerPath)
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]any{}
	oldHook := adapter.Checkpoint
	defer func() { adapter.Checkpoint = oldHook }()
	var begin time.Time
	var commitNS int64
	adapter.Checkpoint = func(stage string) {
		if stage == "before-write" {
			begin = time.Now()
		}
		if stage == "after-dirsync" {
			commitNS = time.Since(begin).Nanoseconds()
		}
	}
	beforeBytes, err := oracle.DiskBytes(catDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = cat.Publish(ctx, a.Repo, na, &a); err != nil {
		t.Fatal(err)
	}
	singleNS := commitNS
	afterBytes, err := oracle.DiskBytes(catDir)
	if err != nil {
		t.Fatal(err)
	}
	peerAfter, err := os.ReadFile(peerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(peerBefore) != string(peerAfter) {
		t.Fatal("single publication changed unrelated repository ref")
	}
	sets, err := os.ReadDir(filepath.Join(catDir, "sets"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 0 {
		t.Fatal("single publication created set metadata")
	}
	results["single_repository"] = map[string]any{"publish_ns": singleNS, "catalog_bytes_before": beforeBytes, "catalog_bytes_after": afterBytes, "unrelated_ref_unchanged": true, "set_directories_created": 0}
	beforeBytes = afterBytes
	if err = cat.PublishSet(ctx, adapter.View{a.Repo: a, peer.Repo: peer}, adapter.View{a.Repo: na, peer.Repo: peer}); err != nil {
		t.Fatal(err)
	}
	afterBytes, err = oracle.DiskBytes(catDir)
	if err != nil {
		t.Fatal(err)
	}
	sets, err = os.ReadDir(filepath.Join(catDir, "sets"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 {
		t.Fatal("set publication must create one commit record")
	}
	results["linked_set_two_repositories"] = map[string]any{"publish_ns": commitNS, "catalog_bytes_before": beforeBytes, "catalog_bytes_after": afterBytes, "set_directories_created": 1}
	results["limit"] = "One tiny-fixture observation per path, not a performance benchmark or stable latency estimate. Includes prepare-log fsync and commit directory fsync, excludes segment verification."
	if err = os.MkdirAll("verification", 0755); err != nil {
		t.Fatal(err)
	}
	if err = dataset.WriteJSON("verification/refs-path-observations.json", results); err != nil {
		t.Fatal(err)
	}
}
