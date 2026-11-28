package contracts

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
// the registry unchanged. missingCensus is the first unchecked census path.
func JoinRebuildRouterPrefixFacts(reg *Registry, facts map[string]RouterFacts, missingCensus string) (missing string) {
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
		}
		local, cross := RouterPrefixFactUse(c, own)
		if local && !ok {
			noteMissing(c.FilePath)
		}
		if cross {
			noteMissing(missingCensus)
		}
	}
	representatives := reg.All()
	for _, c := range representatives {
		check(c)
	}
	// A representative may open a bucket without using any shared facts. Its
	// eligible ByID siblings still get their own prefix lookups in that case.
	rewrites := collectRouterRewrites(reg, representatives, facts, check)
	if missing == "" {
		applyRouterRewrites(reg, rewrites)
	}
	return missing
}
