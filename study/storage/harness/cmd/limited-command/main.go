package main

import (
	"fmt"
	"gxstudy/runner"
	"os"
)

func main() {
	if err := runner.RunExternal(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
