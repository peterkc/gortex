// These external tests hold the contract suite's backend adapters: one
// storetest.Backend per name in storetest.Backends. The adapters sit
// outside storetest because a backend package's own tests import
// storetest, so storetest cannot import a backend; this package can, and one
// test binary can therefore open every listed backend, as K13's backend pairs
// need.
package storetest_test

import "github.com/zzet/gortex/internal/graph/storetest"

// Lookup returns the adapter of a backend in storetest.Backends. The bool is
// false for a name with no adapter.
func Lookup(name string) (storetest.Backend, bool) {
	switch name {
	case "sqlite":
		return SQLite(), true
	}
	return storetest.Backend{}, false
}
