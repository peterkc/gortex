package storage

// InputManifestVersion is the manifest format this build writes. A reader
// treats a manifest of any other version as unusable, never as empty.
const InputManifestVersion = 1

// InputManifestEntryState is what the sample found at one path.
type InputManifestEntryState string

const (
	// InputManifestPresent: the path exists in the working tree with content
	// that differs from HEAD (or HEAD lacks it).
	InputManifestPresent InputManifestEntryState = "present"
	// InputManifestAbsent: the path is gone from the working tree.
	InputManifestAbsent InputManifestEntryState = "absent"
	// InputManifestOpaque: the path is a symlink, submodule or other entry
	// whose content the sampler does not hash.
	InputManifestOpaque InputManifestEntryState = "opaque"
	// InputManifestHeadEqual: the path was dirty in git's view but its bytes
	// and mode equal HEAD's.
	InputManifestHeadEqual InputManifestEntryState = "head_equal"
)

// InputManifestAdmission is the builder's admission verdict for one path.
type InputManifestAdmission string

const (
	InputManifestAdmitted        InputManifestAdmission = "admitted"
	InputManifestExcluded        InputManifestAdmission = "excluded"
	InputManifestUnknownLanguage InputManifestAdmission = "unknown_language"
	InputManifestOversized       InputManifestAdmission = "oversized"
	InputManifestUnreadable      InputManifestAdmission = "unreadable"
	InputManifestNotApplicable   InputManifestAdmission = "not_applicable"
)

// InputManifestEntry is one path of a generation's admitted-input manifest.
// FilePath is git-relative and canonical (path.Clean, forward slashes), the
// same spelling as the builder's change set.
type InputManifestEntry struct {
	FilePath      string
	State         InputManifestEntryState
	Admission     InputManifestAdmission
	Mode          string
	ContentSHA256 string
}

// InputManifestMeta declares a generation's manifest complete. EntryCount is
// the number of entry rows the manifest carries; WriteInputManifest stamps it
// from the entries it writes and refuses a caller value that disagrees.
type InputManifestMeta struct {
	ManifestVersion int
	IsFull          bool
	EntryCount      int
	PolicyDigest    string
}
