package embedding

import (
	"errors"
	"testing"
)

func TestDisableONNXTelemetryEnvDefaultsOff(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantOff bool
		wantSet string
	}{
		{name: "unset", env: map[string]string{}, wantOff: true, wantSet: "1"},
		{name: "user value replaced", env: map[string]string{ortDisableTelemetryEnv: "0"}, wantOff: true, wantSet: "1"},
		{name: "opt-in other value", env: map[string]string{onnxTelemetryOptInEnv: "yes"}, wantOff: true, wantSet: "1"},
		{name: "opt-in", env: map[string]string{onnxTelemetryOptInEnv: " 1 ", ortDisableTelemetryEnv: "0"}, wantOff: false, wantSet: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Given the environment of the case
			env := tc.env
			getenv := func(k string) string { return env[k] }
			setenv := func(k, v string) error { env[k] = v; return nil }

			// When the backend prepares to load ONNX Runtime
			off, err := disableONNXTelemetryEnv(getenv, setenv)

			// Then telemetry stays off unless the user opted in
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if off != tc.wantOff {
				t.Fatalf("off = %v, want %v", off, tc.wantOff)
			}
			if got := env[ortDisableTelemetryEnv]; got != tc.wantSet {
				t.Fatalf("%s = %q, want %q", ortDisableTelemetryEnv, got, tc.wantSet)
			}
		})
	}
}

func TestDisableONNXTelemetryEnvReportsSetenvFailure(t *testing.T) {
	// Given a setenv that fails
	boom := errors.New("setenv failed")
	getenv := func(string) string { return "" }
	setenv := func(string, string) error { return boom }

	// When the backend prepares to load ONNX Runtime
	_, err := disableONNXTelemetryEnv(getenv, setenv)

	// Then the failure reaches the caller, which must not load the library
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
