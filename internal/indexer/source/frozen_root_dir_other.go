//go:build !unix

package source

import "os"

func openRootFrozenDir(_ *os.Root, _ string) (*os.File, error) {
	return nil, ErrRegularReadUnsupported
}
