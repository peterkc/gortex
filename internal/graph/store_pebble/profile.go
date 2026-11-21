package store_pebble

import (
	"errors"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// Profile determines which graph groups a full private build materializes.
type Profile string

const (
	DefaultProfile Profile = "default"
	FullProfile    Profile = "full"
)

var ErrGroupOff = errors.New("graph group is off")

type groupOffError struct {
	group   string
	profile Profile
}

func (e groupOffError) Error() string {
	return fmt.Sprintf("%s: %s (index.profile=%s)", ErrGroupOff, e.group, e.profile)
}

func (e groupOffError) Unwrap() error { return ErrGroupOff }

func edgeGroup(kind graph.EdgeKind) string {
	switch kind {
	case "defines", "contains", "param_of", "returns":
		return "structure"
	case "reads", "writes", "value_flow", "arg_of", "returns_to", "captures", "accesses_field":
		return "data flow"
	case "owns", "authored", "licensed_as", "generated_by", "co_change", "motivates":
		return "ownership and history"
	case "semantically_related", "cross_repo_motivates":
		return "semantic relations"
	default:
		return ""
	}
}

func (p Profile) keepsEdge(kind graph.EdgeKind) bool {
	return p == FullProfile || edgeGroup(kind) == ""
}

func (p Profile) keepsNode(kind graph.NodeKind) bool {
	if p == FullProfile {
		return true
	}
	switch kind {
	case "local", "param", "generic_param", "closure":
		return false
	default:
		return true
	}
}
