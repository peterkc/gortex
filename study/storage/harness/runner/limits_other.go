//go:build !linux

package runner

import (
	"fmt"
	"os/exec"
)

type memoryScope struct{ path string }

func newMemoryScope(root, label string, limit int64) (*memoryScope, error) {
	return nil, fmt.Errorf("hard memory enforcement requires Linux cgroup v2")
}
func (s *memoryScope) close() error {
	return fmt.Errorf("hard memory enforcement requires Linux cgroup v2")
}
func limitCommand(cmd *exec.Cmd, scope string) (func(), error) {
	return nil, fmt.Errorf("hard memory enforcement requires Linux cgroup v2")
}

func (s *memoryScope) stop() error {
	return fmt.Errorf("hard memory enforcement requires Linux cgroup v2")
}
