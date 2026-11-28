package contracts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestSpringConfigValueBindingRelaxed proves the Spring config-key graph:
// application.yml leaf keys become value-redacted config-key nodes, a camelCase
// @Value reference binds to a kebab-case property key through relaxed
// canonicalization, and an @ConfigurationProperties(prefix) read fans out to
// every key under the prefix — all on the existing reads_config surface so
// "which beans read this property" is one traversal. Values are never stored.
func TestSpringConfigValueBindingRelaxed(t *testing.T) {
	const ymlPath = "src/main/resources/application.yml"
	yml := "app:\n" +
		"  my-prop: super-secret-token\n" +
		"  nested:\n" +
		"    timeout: 30\n" +
		"db:\n" +
		"  url: jdbc:postgres://localhost\n" +
		"  pool-size: 10\n"

	g := graph.New()
	g.AddNode(&graph.Node{ID: ymlPath, Kind: graph.KindFile, Name: ymlPath, FilePath: ymlPath})
	// A @Value bean reading the property in camelCase (`${app.myProp}`).
	g.AddNode(&graph.Node{
		ID: "AppConfig.java::AppConfig", Kind: graph.KindType, Name: "AppConfig",
		FilePath: "AppConfig.java", StartLine: 5,
		Meta: map[string]any{"spring_config_keys": []string{"app.myProp"}},
	})
	// A @ConfigurationProperties(prefix = "db") bean — prefix fanout.
	g.AddNode(&graph.Node{
		ID: "DbConfig.java::DbConfig", Kind: graph.KindType, Name: "DbConfig",
		FilePath: "DbConfig.java", StartLine: 3,
		Meta: map[string]any{"spring_config_keys": []string{"db.*"}},
	})

	root := t.TempDir()
	configPath := filepath.Join(root, filepath.FromSlash(ymlPath))
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	if n, err := BindSpringConfig(g, SpringConfigScope{RepoRoot: root}); err != nil || n == 0 {
		t.Fatalf("BindSpringConfig added nothing (err=%v)", err)
	}

	// Config-key nodes exist, canonicalized, value-redacted.
	key := g.GetNode("cfg::spring::app.myprop")
	if key == nil {
		t.Fatal("missing config-key node cfg::spring::app.myprop")
	}
	if key.Kind != graph.KindConfigKey {
		t.Errorf("app.my-prop node kind=%v, want config_key", key.Kind)
	}
	if r, _ := key.Meta["value_redacted"].(bool); !r {
		t.Error("config-key node is not marked value_redacted")
	}
	if key.Name == "super-secret-token" || key.ID == "super-secret-token" {
		t.Error("a config value leaked into the graph")
	}
	if key.Name != "app.my-prop" {
		t.Errorf("config-key Name=%q, want the raw key app.my-prop", key.Name)
	}

	// Collect reads_config edges by (from, to).
	type rc struct{ from, to string }
	reads := map[rc]bool{}
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeReadsConfig {
			reads[rc{e.From, e.To}] = true
		}
	}

	// Relaxed binding: camelCase @Value `app.myProp` binds to kebab key app.my-prop.
	if !reads[rc{"AppConfig.java::AppConfig", "cfg::spring::app.myprop"}] {
		t.Error("relaxed @Value binding missing: AppConfig should read cfg::spring::app.myprop")
	}
	// Prefix fanout: @ConfigurationProperties(prefix=db) reads every db.* key.
	for _, want := range []string{"cfg::spring::db.url", "cfg::spring::db.poolsize"} {
		if !reads[rc{"DbConfig.java::DbConfig", want}] {
			t.Errorf("prefix fanout missing: DbConfig should read %s", want)
		}
	}
}

// TestSpringConfigFileDetection covers the application(-profile)? file matcher
// and the .properties key parse path.
func TestSpringConfigFileDetection(t *testing.T) {
	cases := []struct {
		path        string
		wantOK      bool
		wantProfile string
	}{
		{"application.yml", true, ""},
		{"src/main/resources/application.yaml", true, ""},
		{`src\main\resources\application.yml`, true, ""},
		{"application-prod.properties", true, "prod"},
		{"config/application-staging.yml", true, "staging"},
		{"other.yml", false, ""},
		{"app/application.go", false, ""},
	}
	for _, c := range cases {
		profile, ok := SpringConfigProfile(c.path)
		if ok != c.wantOK || profile != c.wantProfile {
			t.Errorf("SpringConfigProfile(%q) = (%q,%v), want (%q,%v)", c.path, profile, ok, c.wantProfile, c.wantOK)
		}
	}

	keys := parsePropertiesKeys([]byte("# comment\napp.name=svc\nserver.port = 8080\n\n!bang=ignored\n"))
	got := map[string]bool{}
	for _, k := range keys {
		got[k] = true
	}
	if !got["app.name"] || !got["server.port"] {
		t.Errorf("parsePropertiesKeys missing keys, got %v", keys)
	}
	if got["!bang"] {
		t.Error("parsePropertiesKeys did not skip a !-comment")
	}
}
