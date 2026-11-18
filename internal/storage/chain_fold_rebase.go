package storage

// RebaseViewGenerationRequest moves one generation from one base to another.
type RebaseViewGenerationRequest struct {
	GenerationID int64
	FromBase     int64
	ToBase       int64
}
