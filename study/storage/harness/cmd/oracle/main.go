package main

import (
	"gxstudy/oracle"
	"gxstudy/runner"
)

func main() { b := oracle.Backend{}; runner.Main(b, runner.ReferenceCatalog(b)) }
