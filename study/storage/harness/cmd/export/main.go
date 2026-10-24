// Export uses only the installed sqlite3 CLI; source databases must be copies.
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"gxstudy/dataset"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type sourceFlags []string

func (s *sourceFlags) String() string     { return strings.Join(*s, ";") }
func (s *sourceFlags) Set(v string) error { *s = append(*s, v); return nil }
func query[T any](db, sql string) ([]T, error) {
	cmd := exec.CommandContext(context.Background(), "sqlite3", "-readonly", "-json", "file:"+db+"?mode=ro&immutable=1", sql)
	b, e := cmd.Output()
	if e != nil {
		return nil, fmt.Errorf("sqlite3: %w", e)
	}
	var rows []T
	if len(b) > 0 {
		e = json.Unmarshal(b, &rows)
	}
	return rows, e
}

// fullRows preserves every SQLite column, including generated fields and nulls.
// BLOB meta is read as hex to avoid sqlite3 JSON's invalid UTF-8 BLOB path, then
// represented as base64 in the neutral payload.
func fullRows(db, table, order string) ([]map[string]json.RawMessage, error) {
	columns, err := query[struct {
		Name string `json:"name"`
	}](db, "PRAGMA table_xinfo("+table+")")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, col := range columns {
		name := `"` + strings.ReplaceAll(col.Name, `"`, `""`) + `"`
		if col.Name == "meta" {
			name = "CASE WHEN meta IS NULL THEN NULL ELSE hex(meta) END AS meta"
		}
		names = append(names, name)
	}
	rows, err := query[map[string]json.RawMessage](db, "SELECT "+strings.Join(names, ",")+" FROM "+table+" ORDER BY "+order)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if string(row["meta"]) != "null" && len(row["meta"]) > 0 {
			var text string
			if err = json.Unmarshal(row["meta"], &text); err != nil {
				return nil, err
			}
			b, err := hex.DecodeString(text)
			if err != nil {
				return nil, err
			}
			row["meta"], err = json.Marshal(base64.StdEncoding.EncodeToString(b))
			if err != nil {
				return nil, err
			}
		}
	}
	return rows, nil
}

type SourceRoot struct {
	ID       string `json:"id"`
	Tree     string `json:"tree"`
	Revision string `json:"revision"`
}

func export(db, tree, repo, revision, out string) error {
	return exportWithRoots(db, tree, repo, revision, out, nil)
}
func exportWithRoots(db, tree, repo, revision, out string, roots map[string]SourceRoot) error {
	d := dataset.Dataset{Repo: repo, Meta: dataset.Manifest{Version: dataset.GeneratorVersion, Seed: 20261022, Origin: "real", SourceRevision: revision}}
	prefixes, err := query[struct {
		Prefix string `json:"prefix"`
	}](db, `SELECT DISTINCT repo_prefix AS prefix FROM files ORDER BY repo_prefix`)
	if err != nil {
		return err
	}
	if len(prefixes) == 0 {
		return fmt.Errorf("store contains no repository prefixes")
	}
	generations, err := query[struct {
		Generation int64 `json:"generation"`
	}](db, `SELECT DISTINCT view_gen AS generation FROM nodes ORDER BY view_gen`)
	if err != nil {
		return err
	}
	if len(generations) != 1 {
		return fmt.Errorf("export requires one materialized node generation; got %d", len(generations))
	}
	sourcePrefix := prefixes[0].Prefix
	var e error
	d.Meta.SourceSHA256, e = dataset.Hash(db)
	if e != nil {
		return e
	}
	nodeRows, err := fullRows(db, "nodes", "id")
	if err != nil {
		return err
	}
	for _, row := range nodeRows {
		b, err := json.Marshal(row)
		if err != nil {
			return err
		}
		var n dataset.Node
		if err = json.Unmarshal(b, &n); err != nil {
			return err
		}
		for _, key := range []string{"id", "kind", "name", "qual_name", "file_path", "start_line", "end_line", "language"} {
			delete(row, key)
		}
		n.Payload = row
		d.Nodes = append(d.Nodes, n)
	}
	edgeRows, err := fullRows(db, "edges", "from_id,to_id,kind,file_path,line")
	if err != nil {
		return err
	}
	for _, row := range edgeRows {
		row["from"] = row["from_id"]
		row["to"] = row["to_id"]
		var cross int
		if err = json.Unmarshal(row["cross_repo"], &cross); err != nil {
			return err
		}
		row["cross_repo"], _ = json.Marshal(cross != 0)
		b, err := json.Marshal(row)
		if err != nil {
			return err
		}
		var edge dataset.Edge
		if err = json.Unmarshal(b, &edge); err != nil {
			return err
		}
		for _, key := range []string{"from", "to", "from_id", "to_id", "kind", "file_path", "line", "cross_repo", "confidence"} {
			delete(row, key)
		}
		edge.Payload = row
		d.Edges = append(d.Edges, edge)
	}
	d.Files, e = query[dataset.File](db, `SELECT file_path AS path,content_hash,size FROM files ORDER BY file_path`)
	if e != nil {
		return e
	}
	d.Symbols, e = query[dataset.Symbol](db, `SELECT node_id,tokens FROM symbol_fts ORDER BY node_id`)
	if e != nil {
		return e
	}
	if len(prefixes) == 1 {
		cfg, ok := roots[sourcePrefix]
		if ok {
			if cfg.ID != "" {
				d.Repo = cfg.ID
			}
			if cfg.Tree != "" {
				tree = cfg.Tree
			}
			if cfg.Revision != "" {
				d.Meta.SourceRevision = cfg.Revision
			}
		}
		return finishExport(d, tree, sourcePrefix, out)
	}
	fileOwners, err := query[struct {
		Path   string `json:"path"`
		Prefix string `json:"prefix"`
	}](db, `SELECT file_path AS path,repo_prefix AS prefix FROM files`)
	if err != nil {
		return err
	}
	files := map[string]string{}
	for _, f := range fileOwners {
		files[f.Path] = f.Prefix
	}
	prefixesList := make([]string, len(prefixes))
	labels := map[string]string{}
	configs := map[string]SourceRoot{}
	for i, p := range prefixes {
		prefixesList[i] = p.Prefix
		cfg := roots[p.Prefix]
		if cfg.ID == "" {
			cfg.ID = fmt.Sprintf("%s-%03d", repo, i)
		}
		if cfg.Tree == "" {
			cfg.Tree = filepath.Join(tree, p.Prefix)
		}
		if cfg.Revision == "" {
			cfg.Revision = revision
		}
		if cfg.ID == "" || cfg.ID == ".." || strings.ContainsAny(cfg.ID, "/\\") {
			return fmt.Errorf("invalid output repository label")
		}
		labels[p.Prefix] = cfg.ID
		configs[p.Prefix] = cfg
	}
	owners := map[string]string{}
	for _, n := range d.Nodes {
		var prefix string
		_ = json.Unmarshal(n.Payload["repo_prefix"], &prefix)
		if prefix == "" {
			prefix = files[n.FilePath]
		}
		if prefix == "" {
			prefix = idPrefix(n.ID, prefixesList)
		}
		owners[n.ID] = prefix
	}
	// Real ownership and physical assignment are separate. Unowned external
	// nodes are assigned once, to the lowest-labelled referencing repository.
	// They never create a semantic cross-repository edge merely by placement.
	assigned := map[string]string{}
	for id, owner := range owners {
		if labels[owner] != "" {
			assigned[id] = owner
		}
	}
	choose := func(id, candidate string) {
		if labels[candidate] == "" || owners[id] != "" {
			return
		}
		current := assigned[id]
		if current == "" || labels[candidate] < labels[current] {
			assigned[id] = candidate
		}
	}
	for _, edge := range d.Edges {
		sourceOwner, targetOwner := owners[edge.From], owners[edge.To]
		choose(edge.To, sourceOwner)
		choose(edge.From, targetOwner)
		choose(edge.From, files[edge.FilePath])
		choose(edge.To, files[edge.FilePath])
	}
	fallback := prefixesList[0]
	for _, prefix := range prefixesList {
		if labels[prefix] < labels[fallback] {
			fallback = prefix
		}
	}
	for _, node := range d.Nodes {
		if assigned[node.ID] == "" {
			assigned[node.ID] = fallback
		}
	}
	parts := map[string]dataset.Dataset{}
	for _, prefix := range prefixesList {
		part := dataset.Dataset{Repo: labels[prefix], Meta: d.Meta}
		part.Meta.Namespace = labels[prefix]
		part.Meta.SourceRevision = configs[prefix].Revision
		part.Meta.Counts = dataset.Counts{}
		parts[prefix] = part
	}
	qualify := func(id, owner string) string {
		if labels[owner] == "" {
			owner = fallback
		}
		return labels[owner] + "::" + id
	}
	for _, prefix := range prefixesList {
		part := parts[prefix]
		for _, node := range d.Nodes {
			if assigned[node.ID] == prefix {
				node.ID = qualify(node.ID, prefix)
				part.Nodes = append(part.Nodes, node)
			}
		}
		for _, file := range d.Files {
			if files[file.Path] == prefix {
				part.Files = append(part.Files, file)
			}
		}
		for _, sym := range d.Symbols {
			owner := assigned[sym.NodeID]
			if owner == "" {
				owner = fallback
			}
			if owner == prefix {
				sym.NodeID = qualify(sym.NodeID, prefix)
				part.Symbols = append(part.Symbols, sym)
			}
		}
		parts[prefix] = part
	}
	for _, edge := range d.Edges {
		sourceReal, targetReal := owners[edge.From], owners[edge.To]
		edge.CrossRepo = labels[sourceReal] != "" && labels[targetReal] != "" && sourceReal != targetReal
		owner := assigned[edge.From]
		if owner == "" {
			owner = files[edge.FilePath]
		}
		if owner == "" {
			owner = idPrefix(edge.From, prefixesList)
		}
		if labels[owner] == "" {
			owner = fallback
		}
		target := assigned[edge.To]
		if target == "" {
			target = idPrefix(edge.To, prefixesList)
		}
		if target == "" {
			target = owner
		}
		edge.From = qualify(edge.From, owner)
		edge.To = qualify(edge.To, target)
		part := parts[owner]
		part.Edges = append(part.Edges, edge)
		parts[owner] = part
	}
	for _, prefix := range prefixesList {
		cfg := configs[prefix]
		if err = finishExport(parts[prefix], cfg.Tree, prefix, filepath.Join(out, cfg.ID)); err != nil {
			return err
		}
	}
	return nil
}
func idPrefix(id string, prefixes []string) string {
	for _, prefix := range prefixes {
		if strings.HasPrefix(id, prefix+"/") || strings.HasPrefix(id, prefix+"::") {
			return prefix
		}
	}
	return ""
}
func finishExport(d dataset.Dataset, tree, sourcePrefix, out string) error {
	root, e := filepath.EvalSymlinks(tree)
	if e != nil {
		return e
	}
	root, e = filepath.Abs(root)
	if e != nil {
		return e
	}
	cache := map[string][]string{}
	bad := map[string]bool{}
	for _, n := range d.Nodes {
		if n.Kind != "function" && n.Kind != "method" {
			continue
		}
		d.Vectors = append(d.Vectors, dataset.MakeVector(n.ID, 20261022))
		skip := func() { d.Meta.Counts.SkippedSpans++ }
		if n.FilePath == "" || n.StartLine <= 0 || n.EndLine < n.StartLine {
			skip()
			continue
		}
		lines, ok := cache[n.FilePath]
		if !ok && !bad[n.FilePath] {
			relative := strings.TrimPrefix(n.FilePath, sourcePrefix+"/")
			if relative == n.FilePath {
				relative = strings.TrimPrefix(relative, filepath.Base(root)+"/")
			}
			p, e := filepath.EvalSymlinks(filepath.Join(root, relative))
			if e != nil {
				bad[n.FilePath] = true
				skip()
				continue
			}
			rel, e := filepath.Rel(root, p)
			if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				bad[n.FilePath] = true
				skip()
				continue
			}
			b, e := os.ReadFile(p)
			if e != nil {
				bad[n.FilePath] = true
				skip()
				continue
			}
			lines = strings.Split(string(b), "\n")
			cache[n.FilePath] = lines
		}
		if bad[n.FilePath] || n.EndLine > len(lines) {
			skip()
			continue
		}
		ordinal := 0
		for start := n.StartLine - 1; start < n.EndLine; start += 160 {
			end := min(start+160, n.EndLine)
			body := strings.Join(lines[start:end], "\n")
			d.Content = append(d.Content, dataset.Content{ChunkID: fmt.Sprintf("%s#%d", n.ID, ordinal), NodeID: n.ID, FilePath: n.FilePath, Body: body})
			ordinal++
		}
	}
	m, e := dataset.Write(out, d)
	if e == nil {
		fmt.Printf("%s: %+v\n", d.Repo, m.Counts)
	}
	return e
}
func main() {
	db := flag.String("db", "", "copied source SQLite file")
	tree := flag.String("tree", "", "indexed source tree")
	repo := flag.String("repo", "src", "repository ID/path prefix")
	rev := flag.String("revision", "", "source revision")
	out := flag.String("out", "", "output directory")
	rootsFile := flag.String("roots", "", "optional JSON mapping source prefixes to {id,tree,revision}")
	var sources sourceFlags
	flag.Var(&sources, "source", "repeat id,store-copy,source-tree,revision; writes out/id")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(2)
	}
	var e error
	var roots map[string]SourceRoot
	if *rootsFile != "" {
		e = dataset.ReadJSON(*rootsFile, &roots)
		if e != nil {
			fmt.Fprintln(os.Stderr, "cannot load source-root mapping")
			os.Exit(1)
		}
	}
	if len(sources) > 0 {
		for _, s := range sources {
			p := strings.Split(s, ",")
			if len(p) != 4 {
				e = fmt.Errorf("source must have four comma-separated fields")
				break
			}
			e = exportWithRoots(p[1], p[2], p[0], p[3], filepath.Join(*out, p[0]), roots)
			if e != nil {
				break
			}
		}
	} else {
		e = exportWithRoots(*db, *tree, *repo, *rev, *out, roots)
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
