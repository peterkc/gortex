package store_pebble

type RebuildCallReturnSite struct {
	RepoPrefix, FilePath, SymbolID, Name string
}

type RebuildCallReturnBinding struct {
	TypeID   string
	Repeated bool
}

func (s RebuildCallReturnSite) key() []byte {
	return rebuildContractRecordKey('b', s.RepoPrefix, s.FilePath, s.SymbolID, s.Name)
}

type rebuildSavedCallBinding struct {
	RebuildCallReturnBinding
	found bool
}

func readRebuildCallReturnBinding(raw []byte) (rebuildSavedCallBinding, error) {
	d := recordCursor{buf: raw}
	binding := RebuildCallReturnBinding{TypeID: d.str(), Repeated: d.boolean()}
	return rebuildSavedCallBinding{RebuildCallReturnBinding: binding, found: true}, d.finish()
}

// PrepareCallReturnBinding records a frozen representative's lookup, even if
// it missed. Hidden siblings and later writes do not open additional sites.
func (r *RebuildContractRecords) PrepareCallReturnBinding(site RebuildCallReturnSite, resolve func() (RebuildCallReturnBinding, error)) error {
	if err := r.ready(); err != nil {
		return err
	}
	key := site.key()
	known, err := pendingGet(r.build.pending, key, readRebuildCallReturnBinding)
	if err != nil || known.found {
		return err
	}
	binding, err := resolve()
	if err != nil {
		return err
	}
	e := contractFactsEncoder{}
	e.strings(binding.TypeID)
	e.boolean(binding.Repeated)
	return r.build.putRebuildScratch(key, e.buf, goldBufferBytes, "contract call return binding")
}

// CallReturnBinding returns the pre-write native binding answer, if opened.
func (r *RebuildContractRecords) CallReturnBinding(site RebuildCallReturnSite) (RebuildCallReturnBinding, error) {
	if err := r.ready(); err != nil {
		return RebuildCallReturnBinding{}, err
	}
	known, err := pendingGet(r.build.pending, site.key(), readRebuildCallReturnBinding)
	return known.RebuildCallReturnBinding, err
}
