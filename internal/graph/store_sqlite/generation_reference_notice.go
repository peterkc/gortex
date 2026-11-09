package store_sqlite

import "github.com/zzet/gortex/internal/storage"

// EmitGenerationReferencesReleased reports a committed release through the
// existing process-wide observer slot. The backend owns commit timing.
func EmitGenerationReferencesReleased(release GenerationReferenceRelease) {
	storage.EmitGenerationReferencesReleased(release)
}
