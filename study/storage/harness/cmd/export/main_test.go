package main

import (
	"encoding/json"
	"gxstudy/dataset"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestTwoPrefixExportKeepsRealCrossEdges(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "store.sqlite")
	sql := `CREATE TABLE nodes(id TEXT,view_gen INTEGER,kind TEXT,name TEXT,qual_name TEXT,file_path TEXT,start_line INTEGER,end_line INTEGER,language TEXT,repo_prefix TEXT,doc TEXT,signature TEXT,meta BLOB);
CREATE TABLE edges(id INTEGER,from_id TEXT,to_id TEXT,kind TEXT,file_path TEXT,line INTEGER,cross_repo INTEGER,confidence REAL,origin TEXT,meta BLOB);
CREATE TABLE files(repo_prefix TEXT,file_path TEXT,content_hash TEXT,size INTEGER);
CREATE TABLE symbol_fts(node_id TEXT,tokens TEXT);
INSERT INTO nodes VALUES('a/f.go::A',0,'function','A','A','a/f.go',1,1,'go','a','doc-a','func A()',X'010203');
INSERT INTO nodes VALUES('b/f.go::B',0,'function','B','B','b/f.go',1,1,'go','b','doc-b','func B()',X'040506');
INSERT INTO nodes VALUES('dep::helper',0,'function','Helper','Helper','',0,0,'go','','external','helper()',X'0B0C');
INSERT INTO files VALUES('a','a/f.go','a',12),('b','b/f.go','b',12);
INSERT INTO symbol_fts VALUES('a/f.go::A','A graph'),('b/f.go::B','B graph');
INSERT INTO edges VALUES(1,'a/f.go::A','b/f.go::B','calls','a/f.go',1,0,.75,'parser',X'0708');
INSERT INTO edges VALUES(2,'b/f.go::B','a/f.go::A','calls','b/f.go',1,1,.9,'resolver',X'090A');
INSERT INTO edges VALUES(3,'a/f.go::A','dep::helper','calls','a/f.go',1,1,.8,'parser',NULL);
INSERT INTO edges VALUES(4,'b/f.go::B','dep::helper','calls','b/f.go',1,1,.8,'parser',NULL);`
	if out, err := exec.Command("sqlite3", db, sql).CombinedOutput(); err != nil {
		t.Fatalf("fixture database: %v %s", err, out)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "f.go"), []byte("func fixture() {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "exported")
	roots := map[string]SourceRoot{"a": {ID: "labelA", Tree: filepath.Join(dir, "a"), Revision: "revA"}, "b": {ID: "labelB", Tree: filepath.Join(dir, "b"), Revision: "revB"}}
	if err := exportWithRoots(db, dir, "group", "unused", out, roots); err != nil {
		t.Fatal(err)
	}
	a, err := dataset.Load(filepath.Join(out, "labelA"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := dataset.Load(filepath.Join(out, "labelB"))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Nodes)+len(b.Nodes) != 3 || len(a.Edges) != 2 || len(b.Edges) != 2 {
		t.Fatal("prefix split or cross flags incorrect")
	}
	var realA, realB dataset.Node
	var externalID string
	externalCount := 0
	crossCount := 0
	for _, part := range []dataset.Dataset{a, b} {
		for _, n := range part.Nodes {
			if n.Name == "A" {
				realA = n
			}
			if n.Name == "B" {
				realB = n
			}
			if n.Name == "Helper" {
				externalCount++
				externalID = n.ID
			}
		}
	}
	for _, part := range []dataset.Dataset{a, b} {
		for _, e := range part.Edges {
			if e.CrossRepo {
				crossCount++
				if e.To != realA.ID && e.To != realB.ID {
					t.Fatal("unowned endpoint counted as cross")
				}
			} else if e.To != externalID {
				t.Fatal("unowned reference namespace does not resolve")
			}
		}
	}
	if externalCount != 1 || crossCount != 2 {
		t.Fatal("unowned nodes duplicated or cross flags inflated", externalCount, crossCount)
	}
	var meta string
	if err = json.Unmarshal(realA.Payload["meta"], &meta); err != nil || meta != "AQID" {
		t.Fatal("base64 meta lost", meta, err)
	}
	if len(a.Content) != 1 || len(b.Content) != 1 {
		t.Fatal("full edge payload or source chunk lost")
	}
	var cross dataset.Edge
	for _, edge := range a.Edges {
		if edge.CrossRepo {
			cross = edge
		}
	}
	derivedA := dataset.Derive(a, a.Repo, 1, 7)
	derivedB := dataset.Derive(b, b.Repo, 1, 7)
	found := false
	for _, edge := range derivedA.Edges {
		if edge.CrossRepo && edge.To == realB.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("real source tier renames cross target incorrectly")
	}
	originalTarget := cross.To
	local := cross
	local.CrossRepo = false
	local.To = realA.ID
	edges := make([]dataset.Edge, 99)
	for i := range edges {
		edges[i] = local
	}
	edges = append(edges, cross)
	derivedA.Edges = edges
	pair := []dataset.Dataset{derivedA, derivedB}
	dataset.LinkCrossRepo(pair)
	if pair[0].Edges[99].To != originalTarget || !pair[0].Edges[99].CrossRepo {
		t.Fatal("real cross edge overwritten")
	}
}
