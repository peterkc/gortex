package embedding

import "strings"

// ONNX Runtime 1.29 and later send usage telemetry by default on Linux and
// macOS. Gortex keeps it off unless the user opts in with
// GORTEX_ONNX_TELEMETRY=1. Only the embeddings_onnx backend loads the library.
const (
	onnxTelemetryOptInEnv  = "GORTEX_ONNX_TELEMETRY"
	ortDisableTelemetryEnv = "ORT_DISABLE_TELEMETRY"
)

// onnxTelemetryAllowed reports whether the user opted in to ONNX Runtime
// telemetry.
func onnxTelemetryAllowed(getenv func(string) string) bool {
	return strings.TrimSpace(getenv(onnxTelemetryOptInEnv)) == "1"
}

// disableONNXTelemetryEnv sets ORT_DISABLE_TELEMETRY=1 unless the user opted
// in, and reports whether telemetry stays off. Call it before the library
// loads: ONNX Runtime reads the variable from the C environment, which
// os.Setenv also updates in a cgo binary.
func disableONNXTelemetryEnv(getenv func(string) string, setenv func(string, string) error) (bool, error) {
	if onnxTelemetryAllowed(getenv) {
		return false, nil
	}
	if err := setenv(ortDisableTelemetryEnv, "1"); err != nil {
		return true, err
	}
	return true, nil
}
