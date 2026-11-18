package storage

import "time"

// DurationHistogramBounds is bound once by the backend to its original bounds
// array before use. The pointer preserves array edits and whole-array assignments;
// callers must not replace the binding.
var DurationHistogramBounds *[12]time.Duration
