package contracts

// RouterContractSource reports whether JoinRouterPrefixes includes the route's
// own file in addition to the file-node census.
func RouterContractSource(c Contract) bool {
	fw, _ := c.Meta["framework"].(string)
	return c.Type == ContractHTTP && routePrefixFrameworks[fw]
}

// RouterPrefixFactUse reports whether an unjoined route can use facts from its
// own file or other files. A nil own-file answer leaves both uses possible.
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
	switch fw {
	case "spring", "rails", "laravel":
		return true, false
	case "nestjs":
		return true, own == nil || own.ControllerClass != ""
	default:
		return true, own == nil || own.lineFacts(c.Line).Receivers[fw] != ""
	}
}
