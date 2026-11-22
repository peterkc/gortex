package indexer

import (
	"fmt"
	"strings"
)

func (g rebuildGitInventory) sidecarRow() rebuildSidecarRow {
	return rebuildSidecarRow{Kind: rebuildGitIndexStampRow, Path: []byte("."), Digest: g.stage, GitVersion: g.version}
}

func validRebuildGitVersion(v string) bool {
	const prefix = "git version "
	return len(v) > len(prefix) && len(v) < 128 && strings.HasPrefix(v, prefix) &&
		!strings.ContainsFunc(v, func(c rune) bool { return c < ' ' || c > '~' })
}

func (r rebuildSidecarRow) checkGitPolicy() error {
	if r.Present || len(r.Dirs) != 0 || r.Config != nil || r.NPM != nil || r.Classification != nil {
		return fmt.Errorf("%w: unrelated Git policy fields", errRebuildInputSidecar)
	}
	zero := r.Digest == (rebuildDigest{})
	switch r.Kind {
	case rebuildGitIndexStampRow:
		if string(r.Path) == "." && !zero && validRebuildGitVersion(r.GitVersion) && r.GitVerdict == 0 {
			return nil
		}
	case rebuildGitIgnoreVerdictRow:
		if validRebuildNamespaceKey(strings.TrimSuffix(string(r.Path), "/")) && zero && r.GitVersion == "" &&
			r.GitVerdict >= rebuildGitIgnoreNoMatch && r.GitVerdict <= rebuildGitIgnoreIgnored {
			return nil
		}
	}
	return fmt.Errorf("%w: invalid Git policy row", errRebuildInputSidecar)
}

// Git verdicts are evidence of rule application, not file type or directory
// closure. Checking this stream never reopens Git or its policy files.
type rebuildGitPolicyProof struct {
	stamp   bool
	paths   map[string]struct{}
	keySize int
}

func (p *rebuildGitPolicyProof) add(row rebuildSidecarRow) error {
	switch row.Kind {
	case rebuildGitIndexStampRow:
		if p.stamp {
			return fmt.Errorf("%w: repeated Git index stamp", errRebuildInputSidecar)
		}
		p.stamp = true
	case rebuildGitIgnoreVerdictRow:
		key := strings.TrimSuffix(string(row.Path), "/")
		if _, seen := p.paths[key]; seen {
			return fmt.Errorf("%w: repeated Git verdict for %q", errRebuildInputSidecar, key)
		}
		if len(p.paths) == rebuildNamespaceEntryLimit || len(key) > rebuildNamespaceKeyBytesLimit-p.keySize {
			return fmt.Errorf("%w: Git verdict retention: %w", errRebuildInputSidecar, errRebuildCppCaptureLimit)
		}
		if p.paths == nil {
			p.paths = make(map[string]struct{})
		}
		p.paths[key] = struct{}{}
		p.keySize += len(key)
	}
	return nil
}

func (p *rebuildGitPolicyProof) check(marked bool) error {
	if p.stamp != marked || !p.stamp && len(p.paths) != 0 {
		return fmt.Errorf("%w: Git policy mark and frozen proof disagree", errRebuildInputSidecar)
	}
	return nil
}
