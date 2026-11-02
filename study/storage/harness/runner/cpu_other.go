//go:build !linux

package runner

import "fmt"

// ProcessCPU returns self plus joined-child CPU nanoseconds on Linux.
func ProcessCPU() (int64, error) {
	return 0, fmt.Errorf("measured construction CPU requires Linux")
}
