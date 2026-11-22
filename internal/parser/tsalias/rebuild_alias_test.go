package tsalias

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestFrozenAliasNearestConfig(t *testing.T) {
	const rootConfig = `{"compilerOptions":{"paths":{"@x":["root/x.ts"]}}}`
	const jsConfig = `{"compilerOptions":{"paths":{"@x":["js/x.ts"]}}}`
	const tsConfig = `{"compilerOptions":{"paths":{"@x":["ts/x.ts"]}}}`
	for _, tt := range []struct {
		name, js, ts string
		want         FrozenAliasStem
		configReads  []string
	}{
		{"same directory js first", jsConfig, tsConfig, "app/js/x", []string{"app/jsconfig.json"}},
		{"malformed js", `{`, tsConfig, "app/ts/x", []string{"app/jsconfig.json", "app/tsconfig.json"}},
		{"empty paths falls through", `{"compilerOptions":{"paths":{}}}`, "", "root/x", []string{"app/jsconfig.json", "app/tsconfig.json", "jsconfig.json", "tsconfig.json"}},
		{"baseUrl only shadows", `{"compilerOptions":{"baseUrl":"src","paths":{}}}`, tsConfig, "", []string{"app/jsconfig.json"}},
		{"unused absolute baseUrl still shadows", `{"compilerOptions":{"baseUrl":"/other/checkout"}}`, tsConfig, "", []string{"app/jsconfig.json"}},
		{"all entries dropped still shadows", `{"compilerOptions":{"paths":{"@x":[]}}}`, tsConfig, "", []string{"app/jsconfig.json"}},
		{"extends not followed", "", `{"extends":"../base.json"}`, "root/x", []string{"app/jsconfig.json", "app/tsconfig.json", "jsconfig.json", "tsconfig.json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			raw := map[string]string{
				"tsconfig.json":     rootConfig,
				"app/jsconfig.json": tt.js,
				"app/tsconfig.json": tt.ts,
				"base.json":         `{"compilerOptions":{"paths":{"@inherited":["only.ts"]}}}`,
			}
			for rel, data := range raw {
				if data != "" {
					tsaliasWrite(t, filepath.Join(root, filepath.FromSlash(rel)), data)
				}
			}
			var reads []string
			lookup := func(rel string) (*FrozenAliasConfig, error) {
				reads = append(reads, rel)
				if rel == "base.json" {
					t.Fatal("consulted extends")
				}
				return ParseFrozenAliasConfig([]byte(raw[rel]))
			}
			absent := func(string) (bool, error) { return false, nil }
			got, err := ResolveFrozenAlias("app/main.ts", "@x", lookup, absent)
			m := Load(root).FindForFile("app")
			if native := Resolve(m, "@x"); err != nil || got != tt.want || string(got) != native {
				t.Fatalf("stem = %q, %v; want %q (native %q)", got, err, tt.want, native)
			}
			if !slices.Equal(reads, tt.configReads) {
				t.Fatalf("config reads = %q; want %q", reads, tt.configReads)
			}
			if tt.name == "extends not followed" {
				got, err := ResolveFrozenAlias("app/main.ts", "@inherited", lookup, func(rel string) (bool, error) {
					t.Fatalf("probed inherited target %q", rel)
					return false, nil
				})
				if err != nil || got != "" || Resolve(m, "@inherited") != "" {
					t.Fatalf("inherited-only stem = %q, %v; want miss", got, err)
				}
			}
		})
	}
}

func TestFrozenAliasProbeOrder(t *testing.T) {
	cfg, err := ParseFrozenAliasConfig([]byte(`{"compilerOptions":{"paths":{"@lib/*":["one/*.js","two/*"]}}}`))
	if err != nil || cfg == nil {
		t.Fatalf("parse = %v, %v", cfg, err)
	}
	lookup := func(string) (*FrozenAliasConfig, error) { return cfg, nil }
	wantRaw := func(stem string) []string {
		var probes []string
		for _, suffix := range []string{"", ".ts", "/index.ts", ".tsx", "/index.tsx", ".d.ts", "/index.d.ts", ".js", "/index.js", ".jsx", "/index.jsx", ".mts", "/index.mts", ".cts", "/index.cts", ".mjs", "/index.mjs", ".cjs", "/index.cjs", ".json", "/index.json"} {
			probes = append(probes, stem+suffix)
		}
		return probes
	}
	on, two := wantRaw("one/thing.js"), wantRaw("two/thing")
	for _, tt := range []struct {
		name    string
		present []string
		want    FrozenAliasStem
		probes  []string
	}{
		{"all negatives keep first target", nil, "one/thing", append(slices.Clone(on), two...)},
		{"later target wins", []string{"two/thing/index.ts", "two/thing.tsx"}, "two/thing", append(slices.Clone(on), two[:3]...)},
		{"first true stops all later probes", []string{"one/thing.js.ts", "two/thing"}, "one/thing", on[:2]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var probes []string
			got, err := ResolveFrozenAlias("main.ts", "@lib/thing", lookup, func(rel string) (bool, error) {
				probes = append(probes, rel)
				return slices.Contains(tt.present, rel), nil
			})
			if got != tt.want || err != nil || !slices.Equal(probes, tt.probes) {
				t.Fatalf("stem = %q, %v, probes = %q; want %q, %q", got, err, probes, tt.want, tt.probes)
			}
		})
	}
}

func TestFrozenAliasUnprovedStops(t *testing.T) {
	cfg, err := ParseFrozenAliasConfig([]byte(`{"compilerOptions":{"paths":{"@x":["first","tempting"]}}}`))
	if err != nil || cfg == nil {
		t.Fatalf("parse = %v, %v", cfg, err)
	}
	for _, cause := range []error{ErrFrozenAliasUnproved, errors.New("ignored fixture"), errors.New("outside via fixture symlink")} {
		t.Run(cause.Error(), func(t *testing.T) {
			var probes []string
			got, err := ResolveFrozenAlias("main.ts", "@x", func(string) (*FrozenAliasConfig, error) { return cfg, nil }, func(rel string) (bool, error) {
				probes = append(probes, rel)
				switch rel {
				case "first":
					return false, nil
				case "first.ts":
					return false, cause
				default:
					return true, nil
				}
			})
			if got != "" || !errors.Is(err, cause) || !slices.Equal(probes, []string{"first", "first.ts"}) {
				t.Fatalf("stem = %q, %v, probes = %q; want refusal before later hit", got, err, probes)
			}
			var configs []string
			got, err = ResolveFrozenAlias("app/main.ts", "@x", func(rel string) (*FrozenAliasConfig, error) {
				configs = append(configs, rel)
				return cfg, cause // An error cannot select even a returned row.
			}, func(rel string) (bool, error) {
				t.Fatalf("probed %q after config refusal", rel)
				return true, nil
			})
			if got != "" || !errors.Is(err, cause) || !slices.Equal(configs, []string{"app/jsconfig.json"}) {
				t.Fatalf("stem = %q, %v, config reads = %q; want refusal before later config", got, err, configs)
			}
		})
	}
}

func TestFrozenAliasCanonicalPatterns(t *testing.T) {
	// A longer prefix must keep its native priority even if it sorts later.
	exact, okExact := splitAlias("@x.ts", []string{"exact.ts"})
	wild, okWild := splitAlias("@x*.ts", []string{"wild*.ts"})
	long, okLong := splitAlias("@x.ts*", []string{"long*.ts"})
	if !okExact || !okWild || !okLong {
		t.Fatal("native fixture failed to split")
	}
	// Use a separate exact/wild pair with the same prefix to expose native
	// first-wins. No Go map iteration order is assumed in this witness.
	exactSame, ok := splitAlias("@x", []string{"exact.ts"})
	wildSame, ok2 := splitAlias("@x*", []string{"wild*.ts"})
	if !ok || !ok2 {
		t.Fatal("native same-prefix fixture failed to split")
	}
	if a, b := Resolve(&Map{Entries: []Alias{exactSame, wildSame}}, "@x"), Resolve(&Map{Entries: []Alias{wildSame, exactSame}}, "@x"); a != "exact" || b != "wild" {
		t.Fatalf("forced native orders = %q, %q; want exact, wild", a, b)
	}
	inputs := []string{
		`{"compilerOptions":{"paths":{"@x":["exact.ts"],"@x*":["wild*.ts"],"@x*.ts":["wild*.ts"],"@x.ts":["exact.ts"],"@x.ts*":["long*.ts"]}}}`,
		`{"compilerOptions":{"paths":{"@x.ts*":["long*.ts"],"@x.ts":["exact.ts"],"@x*.ts":["wild*.ts"],"@x*":["wild*.ts"],"@x":["exact.ts"]}}}`,
	}
	for _, raw := range inputs {
		cfg, err := ParseFrozenAliasConfig([]byte(raw))
		want := []Alias{exact, long, exactSame, wildSame, wild}
		if err != nil || cfg == nil || !reflect.DeepEqual(cfg.Entries, want) {
			t.Fatalf("config = %+v, %v; want entries %+v", cfg, err, want)
		}
		got, err := ResolveFrozenAlias("main.ts", "@x", func(string) (*FrozenAliasConfig, error) { return cfg, nil }, func(rel string) (bool, error) {
			if rel != "exact.ts" {
				t.Fatalf("probed noncanonical target %q", rel)
			}
			return true, nil
		})
		if got != "exact" || err != nil {
			t.Fatalf("stem = %q, %v; want exact", got, err)
		}
	}
}

func TestFrozenAliasPortable(t *testing.T) {
	raw := `{
		// The logical config location is independent of the checkout location.
		"compilerOptions":{"baseUrl":"./scratch/../src", "paths":{"@/*":["../lib/*"],},},
	}`
	cfg, err := ParseFrozenAliasConfig([]byte(raw))
	if err != nil || cfg == nil || cfg.BaseURL != "src" {
		t.Fatalf("portable row = %+v, %v; want base src", cfg, err)
	}
	var reads []string
	lookup := func(rel string) (*FrozenAliasConfig, error) {
		reads = append(reads, rel)
		if rel == "packages/app[1]/tsconfig.json" {
			return cfg, nil
		}
		return nil, nil
	}
	got, err := ResolveFrozenAlias("packages/app[1]/main.ts", "@/a", lookup, func(rel string) (bool, error) {
		if rel != "packages/app[1]/lib/a" {
			t.Fatalf("nonliteral probe %q", rel)
		}
		return true, nil
	})
	if err != nil || got != "packages/app[1]/lib/a" || !slices.Equal(reads, []string{"packages/app[1]/jsconfig.json", "packages/app[1]/tsconfig.json"}) {
		t.Fatalf("stem = %q, %v; config reads = %q", got, err, reads)
	}
	for _, leaf := range []string{"checkout", "checkout[1]"} {
		root := filepath.Join(t.TempDir(), leaf)
		tsaliasWrite(t, filepath.Join(root, "packages", "app[1]", "tsconfig.json"), raw)
		tsaliasWrite(t, filepath.Join(root, "packages", "app[1]", "lib", "a"), "export {}")
		if native := Resolve(Load(root).FindForFile("packages/app[1]"), "@/a"); native != string(got) {
			t.Fatalf("native stem in %s = %q; want frozen %q", leaf, native, got)
		}
	}
	// Native joins and cleans the interpolated target before probing.
	got, err = ResolveFrozenAlias("packages/app[1]/main.ts", "@/../../../../outside", lookup, func(rel string) (bool, error) {
		t.Fatalf("probed escaped path %q", rel)
		return true, nil
	})
	if got != "" || !errors.Is(err, ErrFrozenAliasPath) {
		t.Fatalf("escaped stem = %q, %v; want path refusal", got, err)
	}
	for _, raw := range []string{
		`{"compilerOptions":{"baseUrl":"/checkout/internal","paths":{"@x":["x"]}}}`,
		`{"compilerOptions":{"paths":{"@x":["C:/checkout/internal/x"]}}}`,
	} {
		if cfg, err := ParseFrozenAliasConfig([]byte(raw)); cfg != nil || !errors.Is(err, ErrFrozenAliasPath) {
			t.Fatalf("absolute-fragment row = %+v, %v; want path refusal", cfg, err)
		}
	}
}

func TestFrozenAliasConfigCap(t *testing.T) {
	const base = `{"compilerOptions":{"paths":{"@x":["a.ts"]}}}`
	raw := base + strings.Repeat(" ", MaxFrozenAliasConfigBytes-len(base))
	if cfg, err := ParseFrozenAliasConfig([]byte(raw)); err != nil || cfg == nil || len(cfg.Entries) != 1 {
		t.Fatalf("config at cap = %+v, %v; want one alias", cfg, err)
	}
	if cfg, err := ParseFrozenAliasConfig([]byte(raw + " ")); cfg != nil || !errors.Is(err, ErrFrozenAliasConfigLimit) {
		t.Fatalf("config over cap = %+v, %v; want limit refusal", cfg, err)
	}
}
