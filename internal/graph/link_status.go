package graph

// LinkStatus describes the selected target of a Calls edge, not compiler certainty.
type LinkStatus string

const (
	MetaLinkStatus                        = "link_status"
	LinkStatusResolved         LinkStatus = "resolved"
	LinkStatusPlatform         LinkStatus = "platform"
	LinkStatusProbablePlatform LinkStatus = "probable-platform"
	LinkStatusUnresolved       LinkStatus = "unresolved"
)

// LinkStatus derives the state for ordinary targets. A builtin Call must have
// one of the two stored platform statuses; absent or malformed is not proof.
// Non-Calls have no status. This accessor does not validate catalog membership.
func (e *Edge) LinkStatus() (LinkStatus, bool) {
	if e.Kind != EdgeCalls {
		return "", false
	}
	if IsBuiltinStub(e.To) {
		raw, _ := e.Meta[MetaLinkStatus].(string)
		switch state := LinkStatus(raw); state {
		case LinkStatusPlatform, LinkStatusProbablePlatform:
			return state, true
		default:
			return "", false
		}
	}
	if e.To == "" || IsUnresolvedTarget(e.To) {
		return LinkStatusUnresolved, true
	}
	return LinkStatusResolved, true
}
