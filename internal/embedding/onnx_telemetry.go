package embedding

import (
	"errors"
	"fmt"
	"strings"
)

const (
	onnxTelemetryOptInEnv  = "GORTEX_ONNX_TELEMETRY"
	ortDisableTelemetryEnv = "ORT_DISABLE_TELEMETRY"
)

// initializeONNXEnvironment opts out before loading the library. The caller owns
// the environment after success; a failed telemetry-off call destroys it here.
func initializeONNXEnvironment(libPath string, getenv func(string) string, setenv func(string, string) error,
	setLibraryPath func(string), initialize, disableTelemetry, destroy func() error,
) error {
	// Mirror internal/telemetry/consent.go's doNotTrackAsserted rule. Its
	// parser is unexported, and GORTEX_TELEMETRY has separate consent semantics.
	dnt := strings.ToLower(strings.TrimSpace(getenv("DO_NOT_TRACK")))
	telemetryOff := strings.TrimSpace(getenv(onnxTelemetryOptInEnv)) != "1" || (dnt != "" && dnt != "0" && dnt != "false")
	if telemetryOff {
		// os.Setenv also updates the C environment in a cgo binary.
		if err := setenv(ortDisableTelemetryEnv, "1"); err != nil {
			return fmt.Errorf("ONNX Runtime telemetry opt-out: %w", err)
		}
	}
	setLibraryPath(libPath)
	if err := initialize(); err != nil {
		return fmt.Errorf("ONNX Runtime init: %w", err)
	}
	if telemetryOff {
		// This API needs the OrtEnv, so it only stops events after env creation.
		if err := disableTelemetry(); err != nil {
			return errors.Join(fmt.Errorf("ONNX Runtime telemetry opt-out: %w", err), destroy())
		}
	}
	return nil
}
