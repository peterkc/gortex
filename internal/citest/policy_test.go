package citest

import "testing"

func TestPolicyFullLayoutAndInvalidShards(t *testing.T) {
	p, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	full := p.FullMatrix().Include
	if len(full) != 12 {
		t.Fatal(full)
	}
	if err := CheckLayout(full, p); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []string{"indexer:0/2", "indexer:3/2", "rest:1/2", "nope", "indexer-1", "", "mcp+store+mcp", "indexer:01/2"} {
		if _, err := ParsePartition(spec, p); err == nil {
			t.Errorf("invalid partition accepted: %q", spec)
		}
	}
	for name, edit := range map[string]func([]MatrixRow) []MatrixRow{
		"missing shard":   func(rows []MatrixRow) []MatrixRow { return rows[1:] },
		"duplicate shard": func(rows []MatrixRow) []MatrixRow { return append(rows, rows[0]) },
		"mixed counts":    func(rows []MatrixRow) []MatrixRow { rows[1].Partition = "indexer:2/3"; return rows },
		"missing group":   func(rows []MatrixRow) []MatrixRow { rows[2].Partition = "mcp"; return rows },
		"group twice":     func(rows []MatrixRow) []MatrixRow { rows[2].Partition = "mcp+store+mcp"; return rows },
	} {
		t.Run(name, func(t *testing.T) {
			rows := append([]MatrixRow{}, full...)
			if err := CheckLayout(edit(rows), p); err == nil {
				t.Fatal("invalid layout accepted")
			}
		})
	}
}
