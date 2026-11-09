package storage

import "sync/atomic"

// EmitGenerationReferencesReleased reports a committed release through the
// existing process-wide observer slot. The backend owns commit timing.
func EmitGenerationReferencesReleased(release GenerationReferenceRelease) {
	noteGenerationReferencesReleased(release)
}

var generationReferenceReleaseObserver atomic.Pointer[func(GenerationReferenceRelease)]

// OnGenerationReferencesReleased installs the process-wide observer of
// released generation references (nil removes it). It runs after the
// releasing transaction commits, on the writer's goroutine, and must not block.
func OnGenerationReferencesReleased(observer func(GenerationReferenceRelease)) {
	if observer == nil {
		generationReferenceReleaseObserver.Store(nil)
		return
	}
	generationReferenceReleaseObserver.Store(&observer)
}

// noteGenerationReferencesReleased reports a committed release.
func noteGenerationReferencesReleased(release GenerationReferenceRelease) {
	if observer := generationReferenceReleaseObserver.Load(); observer != nil {
		(*observer)(release)
	}
}
