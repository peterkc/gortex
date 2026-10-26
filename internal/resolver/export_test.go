package resolver

import "github.com/zzet/gortex/internal/graph"

// SetGoModuleImportRecoveryBudgetForTest changes only the bounded import budget.
// External resolver tests use it to exercise the real indexer continuation.
func SetGoModuleImportRecoveryBudgetForTest(remaining int) (restore func()) {
	previous := newGoModuleImportRecoveryBudget
	newGoModuleImportRecoveryBudget = func() *graph.IncomingSourceBudget {
		budget := &graph.IncomingSourceBudget{}
		if err := budget.Charge(graph.MaxIncomingSourceCandidateRows - remaining); err != nil {
			panic(err)
		}
		return budget
	}
	return func() { newGoModuleImportRecoveryBudget = previous }
}
