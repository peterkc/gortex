package contracts

import (
	"cmp"
	"maps"
	"slices"
)

// RouterContractSource reports whether JoinRouterPrefixes includes the route's
// own file in addition to the file-node census.
func RouterContractSource(c Contract) bool {
	fw, _ := c.Meta["framework"].(string)
	return c.Type == ContractHTTP && routePrefixFrameworks[fw]
}

// RouterPrefixFactUse reports whether an unjoined route can use facts from its
// own file or other files. A nil own-file answer leaves a supported use possible.
// The test for other files follows prefixForRoute: only a receiver or Nest
// controller that can consult a shared map can use a mount-only file.
func RouterPrefixFactUse(c Contract, own *RouterFacts) (local, crossFile bool) {
	if !RouterContractSource(c) {
		return false, false
	}
	if joined, _ := c.Meta[routePrefixJoinedMeta].(bool); joined {
		return false, false
	}
	fw, _ := c.Meta["framework"].(string)
	var f RouterFacts
	if own != nil {
		f = *own
	}
	key, shared := routerSharedKey(fw, f, f.lineFacts(c.Line))
	return true, shared && (own == nil || key != "")
}

// JoinRebuildRouterPrefixFacts applies the native join only if its selected
// facts are checked. A nonempty return names the first missing file and leaves
// the registry unchanged. uncheckedCensus lists all scanned files without facts.
// On success it returns the sorted files involved in native map-order overlap.
func JoinRebuildRouterPrefixFacts(reg *Registry, facts map[string]RouterFacts, uncheckedCensus []string) (overlapFiles []string, missing string) {
	representatives := reg.All()
	unchecked := make(map[string]bool)
	var missingCensus string
	for _, path := range uncheckedCensus {
		unchecked[path] = true
		if missingCensus == "" || path < missingCensus {
			missingCensus = path
		}
	}
	for _, c := range representatives {
		if _, ok := facts[c.FilePath]; !ok && RouterContractSource(c) {
			unchecked[c.FilePath] = true
		}
	}
	noteMissing := func(path string) {
		if path != "" && (missing == "" || path < missing) {
			missing = path
		}
	}
	check := func(c Contract) {
		f, ok := facts[c.FilePath]
		var own *RouterFacts
		if ok {
			own = &f
		} else if !unchecked[c.FilePath] {
			// Native never scans this hidden sibling's file. Its facts are empty.
			return
		}
		local, cross := RouterPrefixFactUse(c, own)
		if local && !ok {
			noteMissing(c.FilePath)
		}
		if cross {
			noteMissing(missingCensus)
		}
	}
	for _, c := range representatives {
		check(c)
	}
	// A representative may open a bucket without using any shared facts. Its
	// eligible ByID siblings still get their own prefix lookups in that case.
	rewrites := collectRouterRewrites(reg, representatives, facts, check)
	if missing != "" {
		return nil, missing
	}
	overlapFiles = routerRewriteOverlapFiles(rewrites)
	// Native ranges a map. Only rebuild chooses a fixed application order.
	slices.SortFunc(rewrites, func(a, b routerRewrite) int { return cmp.Compare(a.oldID, b.oldID) })
	applyRouterRewrites(reg, rewrites)
	return overlapFiles, ""
}

// A destination that is also a different source can be cleared by the other
// source's rewrite, depending on order. U5 must not count either bucket's
// files as exact native parity, even if a particular run happens to match.
func routerRewriteOverlapFiles(rewrites []routerRewrite) []string {
	bySource := make(map[string][]Contract, len(rewrites))
	for _, rw := range rewrites {
		bySource[rw.oldID] = rw.list
	}
	overlapped := make(map[string]struct{})
	for _, rw := range rewrites {
		for _, c := range rw.list {
			if c.ID != rw.oldID && bySource[c.ID] != nil {
				overlapped[rw.oldID] = struct{}{}
				overlapped[c.ID] = struct{}{}
			}
		}
	}
	files := make(map[string]struct{})
	for id := range overlapped {
		for _, c := range bySource[id] {
			files[c.FilePath] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(files))
}
