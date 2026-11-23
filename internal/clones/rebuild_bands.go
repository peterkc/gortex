package clones

const (
	// BandBucketLimit is the largest native bucket expanded into pairs.
	BandBucketLimit = maxBucketSize
	// CandidateHashLimit is the native per-class dedup cutoff.
	CandidateHashLimit = maxSeenCandidates
)

// VisitStratifiedBandHashes supplies the same class and bucket keys as the
// native in-memory index to a caller that stores postings on disk.
func VisitStratifiedBandHashes(item Item, visit func(class, band int, hash uint64) error) error {
	for _, class := range lengthClassesOf(item.TokenCount) {
		for band := range Bands {
			if err := visit(class, band, bandKey(band, item.Sig)); err != nil {
				return err
			}
		}
	}
	return nil
}

// CandidatePairHash returns the native hash-only admission key for a pair
// whose IDs are in ascending order. It is not a full pair identity.
func CandidatePairHash(a, b string) uint64 { return pairKey(a, b) }
