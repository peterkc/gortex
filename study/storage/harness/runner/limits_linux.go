//go:build linux

package runner

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type memoryScope struct {
	path    string
	info    os.FileInfo
	stopped bool
}

func newMemoryScope(root, label string, limit int64) (*memoryScope, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	if resolved != abs || !strings.HasPrefix(abs, "/sys/fs/cgroup/") || limit <= 0 {
		return nil, fmt.Errorf("explicit delegated cgroup-v2 root and positive limit required")
	}
	kind, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return nil, err
	}
	if !strings.Contains(" "+strings.TrimSpace(string(kind))+" ", " memory ") {
		return nil, fmt.Errorf("memory controller not delegated")
	}
	path, err := os.MkdirTemp(root, "gxstudy-"+label+"-")
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	scope := &memoryScope{path: path, info: info}
	for name, value := range map[string]string{"memory.max": strconv.FormatInt(limit, 10), "memory.swap.max": "0", "memory.oom.group": "1"} {
		if err = os.WriteFile(filepath.Join(path, name), []byte(value), 0600); err != nil {
			return scope, err
		}
		got, e := os.ReadFile(filepath.Join(path, name))
		if e != nil {
			return scope, e
		}
		if strings.TrimSpace(string(got)) != value {
			return scope, fmt.Errorf("kernel limit readback mismatch: %s", name)
		}
	}
	if _, err = os.Stat(filepath.Join(path, "memory.peak")); err != nil {
		return scope, err
	}
	return scope, nil
}
func (s *memoryScope) stop() error {
	if s.stopped {
		return nil
	}
	info, err := os.Lstat(s.path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, s.info) || !info.IsDir() {
		return fmt.Errorf("refuse cleanup of replaced/unowned cgroup")
	}
	file, err := os.Open(filepath.Join(s.path, "cgroup.events"))
	if err != nil {
		return err
	}
	defer file.Close()
	// Read once before the single kill effect to register change notifications.
	if _, err = io.ReadAll(file); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(s.path, "cgroup.kill"), []byte("1"), 0600); err != nil {
		return err
	}
	deadline := time.Now().Add(defaultSafetyTimeout)
	for {
		if _, err = file.Seek(0, 0); err != nil {
			return err
		}
		state, err := io.ReadAll(file)
		if err != nil {
			return err
		}
		if strings.Contains(string(state), "populated 0") {
			s.stopped = true
			return nil
		}
		fd := int(file.Fd())
		var changed syscall.FdSet
		if fd >= len(changed.Bits)*64 {
			return fmt.Errorf("cgroup event fd exceeds select capacity")
		}
		changed.Bits[fd/64] |= int64(1) << uint(fd%64)
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("owned cgroup did not become empty after kill")
		}
		timeout := syscall.NsecToTimeval(remaining.Nanoseconds())
		if _, err = syscall.Select(fd+1, nil, nil, &changed, &timeout); err != nil && err != syscall.EINTR {
			return err
		}
	}
}
func (s *memoryScope) close() error {
	if err := s.stop(); err != nil {
		return err
	}
	info, err := os.Lstat(s.path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, s.info) {
		return fmt.Errorf("refuse removal of replaced/unowned cgroup")
	}
	return os.Remove(s.path)
}
func limitCommand(cmd *exec.Cmd, scope string) (func(), error) {
	if scope == "" {
		return nil, fmt.Errorf("component cgroup missing")
	}
	f, err := os.Open(scope)
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(f.Fd())}
	return func() { f.Close() }, nil
}
