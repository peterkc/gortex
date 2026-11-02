// Command fixture prepares a source-free tiny gxstudy-v2 tier for CI.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
)

const fixtureSeed int64 = 20261022

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("fixture", flag.ContinueOnError)
	out := flags.String("out", "", "required fresh tier directory")
	revision := flags.String("source-revision", "", "required full source commit containing conformance.Fixture")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *out == "" || len(flags.Args()) != 0 {
		return fmt.Errorf("explicit -out and -source-revision required")
	}
	if len(*revision) != 40 && len(*revision) != 64 {
		return fmt.Errorf("full fixture source revision required")
	}
	if _, err := hex.DecodeString(*revision); err != nil {
		return fmt.Errorf("invalid fixture source revision: %w", err)
	}
	// The source is the harness's already-reviewed tiny fixture, not a new
	// dataset format or an export from a local Gortex/SQLite installation.
	d := conformance.Fixture()
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(raw)
	d.Meta = dataset.Manifest{
		Version: dataset.GeneratorVersion, Repo: d.Repo, Origin: "synthetic", Seed: fixtureSeed,
		SourceRevision: *revision, SourceSHA256: hex.EncodeToString(hash[:]),
	}
	if err = os.Mkdir(*out, 0755); err != nil {
		return fmt.Errorf("create fresh fixture tier: %w", err)
	}
	if _, err = dataset.Write(filepath.Join(*out, d.Repo), d); err != nil {
		return err
	}
	queries := workload.Generate([]dataset.Dataset{d}, fixtureSeed)
	segments := map[string]adapter.Segment{d.Repo: oracle.New(d)}
	var expected []workload.Expected
	for _, query := range queries {
		answer, e := workload.Evaluate(context.Background(), segments, nil, query)
		if e != nil {
			return e
		}
		expected = append(expected, workload.Expected{QueryID: query.ID, Answer: answer})
	}
	if err = dataset.WriteJSON(filepath.Join(*out, "queries.json"), queries); err != nil {
		return err
	}
	if err = dataset.WriteJSON(filepath.Join(*out, "expected.json"), expected); err != nil {
		return err
	}
	meta := d.Meta
	meta.Origin = "tier"
	if err = dataset.TierManifest(*out, []string{d.Repo}, meta); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "tiny gxstudy-v2 tier: repos=1 nodes=3 edges=3 queries=%d source=%s\n", len(queries), *revision)
	return err
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
