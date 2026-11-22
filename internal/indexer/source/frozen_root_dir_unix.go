//go:build unix

package source

import (
	"os"

	"golang.org/x/sys/unix"
)

func openRootFrozenDir(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
}
