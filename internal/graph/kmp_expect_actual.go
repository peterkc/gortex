package graph

// KMPKotlinLanguage is the normalized language marker for the KMP pass.
const KMPKotlinLanguage = "kotlin"

// KMPExpectActualKinds returns the declaration kinds eligible for pairing.
func KMPExpectActualKinds() []NodeKind {
	return []NodeKind{KindFunction, KindMethod, KindType, KindInterface}
}

// KMPExpectActualRole returns the role of a named KMP declaration candidate.
// Callers filter by KMPExpectActualKinds before inspecting the role.
func KMPExpectActualRole(n *Node) string {
	if n == nil || n.Name == "" {
		return ""
	}
	role, _ := n.Meta["kmp_role"].(string)
	switch role {
	case "expect", "actual":
		return role
	default:
		return ""
	}
}
