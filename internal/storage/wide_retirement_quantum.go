package storage

import "context"

type payloadWideQuantumKey struct{}

// WithWideRetirementQuantum marks ctx for wide retirement quanta: the caller
// has checked that no interactive work waits for the writer.
func WithWideRetirementQuantum(ctx context.Context) context.Context {
	return context.WithValue(ctx, payloadWideQuantumKey{}, true)
}

// WideRetirementQuantum reports whether ctx is marked for wide retirement quanta.
func WideRetirementQuantum(ctx context.Context) bool {
	wide, _ := ctx.Value(payloadWideQuantumKey{}).(bool)
	return wide
}
