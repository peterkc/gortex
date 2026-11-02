package runner

import "fmt"

// Missing CPU cannot silently become a zero-cost non-empty construction.
func checkBuildCPU(ns, bytes int64) error {
	if ns < 0 || (bytes > 0 && ns == 0) {
		return fmt.Errorf("non-empty build requires positive engine self+children cpu_ns")
	}
	return nil
}
