package embedding

import (
	"errors"
	"slices"
	"testing"
)

func TestInitializeONNXEnvironmentConsent(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantOff bool
		wantSet string
	}{
		{name: "unset", env: map[string]string{}, wantOff: true, wantSet: "1"},
		{name: "user value replaced", env: map[string]string{ortDisableTelemetryEnv: "0"}, wantOff: true, wantSet: "1"},
		{name: "opt-in other value", env: map[string]string{onnxTelemetryOptInEnv: "yes"}, wantOff: true, wantSet: "1"},
		{name: "opt-in", env: map[string]string{onnxTelemetryOptInEnv: " 1 ", ortDisableTelemetryEnv: "0"}, wantSet: "0"},
		{name: "inherited opt-out", env: map[string]string{onnxTelemetryOptInEnv: "1", ortDisableTelemetryEnv: "1"}, wantSet: "1"},
		{name: "do not track", env: map[string]string{onnxTelemetryOptInEnv: "1", "DO_NOT_TRACK": "1"}, wantOff: true, wantSet: "1"},
		{name: "do not track normalized", env: map[string]string{onnxTelemetryOptInEnv: "1", "DO_NOT_TRACK": " TRUE "}, wantOff: true, wantSet: "1"},
		{name: "do not track other value", env: map[string]string{onnxTelemetryOptInEnv: "1", "DO_NOT_TRACK": "no"}, wantOff: true, wantSet: "1"},
		{name: "do not track zero", env: map[string]string{onnxTelemetryOptInEnv: "1", "DO_NOT_TRACK": " 0 "}},
		{name: "do not track false", env: map[string]string{onnxTelemetryOptInEnv: "1", "DO_NOT_TRACK": " FALSE "}},
		{name: "do not track whitespace", env: map[string]string{onnxTelemetryOptInEnv: "1", "DO_NOT_TRACK": " "}},
		{name: "gortex consent independent", env: map[string]string{onnxTelemetryOptInEnv: "1", "GORTEX_TELEMETRY": "0"}},
		{name: "gortex consent cannot override do not track", env: map[string]string{onnxTelemetryOptInEnv: "1", "GORTEX_TELEMETRY": "1", "DO_NOT_TRACK": "1"}, wantOff: true, wantSet: "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			err := initializeONNXEnvironment("runtime", func(k string) string { return tc.env[k] },
				func(k, v string) error { calls = append(calls, "setenv"); tc.env[k] = v; return nil },
				func(path string) {
					calls = append(calls, "library")
					if path != "runtime" {
						t.Fatalf("library path = %q", path)
					}
				},
				func() error { calls = append(calls, "initialize"); return nil },
				func() error { calls = append(calls, "disable"); return nil },
				func() error { calls = append(calls, "destroy"); return nil })
			if err != nil {
				t.Fatalf("initialize: %v", err)
			}
			wantCalls := []string{"library", "initialize"}
			if tc.wantOff {
				wantCalls = []string{"setenv", "library", "initialize", "disable"}
			}
			if !slices.Equal(calls, wantCalls) {
				t.Fatalf("calls = %v, want %v", calls, wantCalls)
			}
			if got := tc.env[ortDisableTelemetryEnv]; got != tc.wantSet {
				t.Fatalf("%s = %q, want %q", ortDisableTelemetryEnv, got, tc.wantSet)
			}
		})
	}
}

func TestInitializeONNXEnvironmentFailures(t *testing.T) {
	boom := errors.New("operation failed")
	cleanupErr := errors.New("cleanup failed")
	cases := []struct {
		name       string
		failAt     string
		destroyErr error
		wantCalls  []string
	}{
		{name: "setenv", failAt: "setenv", wantCalls: []string{"setenv"}},
		{name: "initialize", failAt: "initialize", wantCalls: []string{"setenv", "library", "initialize"}},
		{name: "disable", failAt: "disable", wantCalls: []string{"setenv", "library", "initialize", "disable", "destroy"}},
		{name: "disable and destroy", failAt: "disable", destroyErr: cleanupErr, wantCalls: []string{"setenv", "library", "initialize", "disable", "destroy"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			record := func(step string) error {
				calls = append(calls, step)
				if step == tc.failAt {
					return boom
				}
				return nil
			}
			err := initializeONNXEnvironment("runtime", func(string) string { return "" },
				func(string, string) error { return record("setenv") },
				func(string) { calls = append(calls, "library") },
				func() error { return record("initialize") },
				func() error { return record("disable") },
				func() error { calls = append(calls, "destroy"); return tc.destroyErr })
			if !errors.Is(err, boom) {
				t.Fatalf("error = %v, want original %v", err, boom)
			}
			if tc.destroyErr != nil && !errors.Is(err, tc.destroyErr) {
				t.Fatalf("error = %v, want cleanup error %v too", err, tc.destroyErr)
			}
			if !slices.Equal(calls, tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", calls, tc.wantCalls)
			}
		})
	}
}
