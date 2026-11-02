//go:build linux

package runner

import "syscall"

// Build is serial in a measurement engine. Child sort/compression CPU belongs
// to the same build; runner/checker processes are not its children.
// ProcessCPU returns self plus joined-child CPU nanoseconds on Linux.
func ProcessCPU() (int64, error) {
	var self, children syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &self); err != nil {
		return 0, err
	}
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &children); err != nil {
		return 0, err
	}
	return self.Utime.Nano() + self.Stime.Nano() + children.Utime.Nano() + children.Stime.Nano(), nil
}
