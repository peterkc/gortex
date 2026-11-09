package store_sqlite

// EmitGenerationReferencesReleased reports a committed release through the
// existing process-wide observer slot. The backend owns commit timing.
func EmitGenerationReferencesReleased(release GenerationReferenceRelease) {
	noteGenerationReferencesReleased(release)
}
