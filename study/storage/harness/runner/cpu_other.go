//go:build !linux

package runner

import "fmt"

func processCPU() (int64, error) {
	return 0, fmt.Errorf("measured construction CPU requires Linux")
}
