package store_pebble

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/contracts"
)

type rebuildRouterMounts struct{ records *RebuildContractRecords }

func rebuildRouterMountKey(column byte, name string) []byte {
	return keyStrings([]byte{'g', 'q', 'c', 'j', column}, name)
}

// JoinCheckedRouterPrefixes merges the selected per-file facts from the
// native census and representative route files. factsAt must distinguish an
// absent proof (nil) from a captured empty fact. No source reads are allowed.
// missing is set only when an absent proof causes the refusal.
func (r *RebuildContractRecords) JoinCheckedRouterPrefixes(repo, workspace string,
	factsAt func(string) (*contracts.RouterFacts, error), overlap func(string) error,
) (missing string, err error) {
	if err := r.ready(); err != nil {
		return "", err
	}
	prefix := rebuildContractRecordKey('j')
	defer func() {
		err = errors.Join(err, r.build.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	m := rebuildRouterMounts{records: r}
	var enabled bool
	if err := r.VisitRepresentatives(func(item RebuildContractRecord) error {
		if item.Type == contracts.ContractHTTP {
			switch item.Meta["framework"] {
			case "fastapi/flask", "express", "nestjs":
				enabled = true
			}
		}
		if contracts.RouterContractSource(item.Contract) {
			return m.put('p', item.FilePath, nil)
		}
		return nil
	}); err != nil || !enabled {
		return "", err
	}
	var census bool
	if err := r.build.RebuildView().VisitFileIdentities(r.ctx, func(path, language, nodeRepo, nodeWorkspace string) error {
		if nodeRepo != repo || workspace != "" && nodeWorkspace != workspace ||
			!contracts.RouterFileCensus(strings.ToLower(path), strings.ToLower(language)) {
			return nil
		}
		census = true
		return m.put('p', path, []byte{1})
	}); err != nil || !census {
		return "", err
	}
	var missingCensus string
	paths := append(prefix, 'p')
	if err := pendingVisit(r.ctx, r.build.pending, paths, func(s row) error {
		path, rest, err := readKeyString(s.key[len(paths):])
		if err != nil || len(rest) != 0 || len(s.value) > 1 {
			return invalid("invalid router fact path key")
		}
		facts, err := factsAt(path)
		if err != nil {
			return err
		}
		if facts == nil {
			if len(s.value) == 1 && missingCensus == "" {
				missingCensus = path
			}
			return nil
		}
		e := contractFactsEncoder{}
		writeFactRouter(&e, facts)
		if e.err != nil {
			return e.err
		}
		if err := m.put('f', path, e.buf); err != nil {
			return err
		}
		return m.merge(facts)
	}); err != nil {
		return "", err
	}
	var firstMissing string
	note := func(path string) {
		if path != "" && (firstMissing == "" || path < firstMissing) {
			firstMissing = path
		}
	}
	err = r.RewriteRouterPrefixes(func(c contracts.Contract) (string, error) {
		facts, err := pendingGet(r.build.pending, rebuildRouterMountKey('f', c.FilePath), func(raw []byte) (*contracts.RouterFacts, error) {
			d := recordCursor{buf: raw}
			facts := readFactRouter(&d)
			return facts, d.finish()
		})
		if err != nil {
			return "", err
		}
		if facts == nil {
			_, selected, err := m.read('p', c.FilePath)
			if err != nil {
				return "", err
			}
			if !selected {
				// A hidden sibling outside the native census has empty facts,
				// not an unchecked answer from a selected file.
				facts = &contracts.RouterFacts{}
			}
		}
		local, cross := contracts.RouterPrefixFactUse(c, facts)
		if cross {
			note(missingCensus)
		}
		if facts == nil {
			if local {
				note(c.FilePath)
			}
			return "", nil
		}
		return m.route(c, *facts)
	}, func() error {
		if firstMissing != "" {
			missing = firstMissing
			return fmt.Errorf("missing checked router proof for %q", missing)
		}
		return nil
	}, overlap)
	return missing, err
}

func (m rebuildRouterMounts) put(column byte, key string, value []byte) error {
	return m.records.build.putRebuildScratch(rebuildRouterMountKey(column, key), value, goldBufferBytes, "router mount evidence")
}

func (m rebuildRouterMounts) read(column byte, key string) (string, bool, error) {
	type entry struct {
		value string
		found bool
	}
	e, err := pendingGet(m.records.build.pending, rebuildRouterMountKey(column, key), func(raw []byte) (entry, error) {
		return entry{value: string(raw), found: true}, nil
	})
	return e.value, e.found, err
}

func (m rebuildRouterMounts) merge(f *contracts.RouterFacts) error {
	for _, column := range []struct {
		code byte
		rows map[string]string
	}{{'s', f.SelfPrefix}, {'m', f.MountPrefix}, {'a', f.MountParent}, {'n', f.ModulePrefix}, {'c', f.ControllerModule}} {
		code := column.code
		for key, value := range column.rows {
			prior, found, err := m.read(code, key)
			if err != nil {
				return err
			}
			if !found || code == 'n' || code == 's' && prior == "" && value != "" {
				if err := m.put(code, key, []byte(value)); err != nil {
					return err
				}
			} else if value != prior && (code == 'm' || code == 's' && value != "" && prior != "") {
				if err := m.put(code-'a'+'A', key, nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (m rebuildRouterMounts) route(c contracts.Contract, facts contracts.RouterFacts) (string, error) {
	var readErr error
	get := func(code byte, key string) string {
		if readErr != nil {
			return ""
		}
		value, _, err := m.read(code, key)
		readErr = err
		return value
	}
	unambiguous := func(code byte, key string) string {
		_, conflicted, err := m.read(code-'a'+'A', key)
		if err != nil {
			readErr = errors.Join(readErr, err)
		}
		if conflicted {
			return ""
		}
		return get(code, key)
	}
	value := contracts.RouterPrefixForRoute(c, facts, func(key string) string {
		return unambiguous('s', key)
	}, func(key string) string {
		return m.chain(key, unambiguous, &readErr)
	}, func(key string) string {
		module, found, err := m.read('c', key)
		if err != nil {
			readErr = errors.Join(readErr, err)
		}
		if !found {
			return ""
		}
		return get('n', module)
	})
	return value, readErr
}

func (m rebuildRouterMounts) chain(key string, value func(byte, string) string, errp *error) string {
	r := m.records
	prefix := append(rebuildContractRecordKey('j'), 'v')
	defer func() {
		*errp = errors.Join(*errp, r.build.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	var suffix string
	for *errp == nil {
		if err := r.ctx.Err(); err != nil {
			*errp = err
			return ""
		}
		_, seen, err := m.read('v', key)
		if err != nil {
			*errp = err
			return ""
		}
		if seen {
			return suffix
		}
		if err := m.put('v', key, nil); err != nil {
			*errp = err
			return ""
		}
		mount := value('m', key)
		parent, found, err := m.read('a', key)
		if err != nil {
			*errp = errors.Join(*errp, err)
			return ""
		}
		self := ""
		if found {
			self = value('s', parent)
		}
		suffix = contracts.PrependRouterMount(self, mount, suffix)
		if len(suffix) > goldBufferBytes {
			*errp = fmt.Errorf("%w: router mount chain", ErrRebuildLinkInputSize)
			return ""
		}
		if !found {
			return suffix
		}
		key = parent
	}
	return ""
}
