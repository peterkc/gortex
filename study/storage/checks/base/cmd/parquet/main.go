package main

import (
	"gxstudy/runner"
	"study/checks/base"
)

func main() { b := base.Backend{SideIndex: false}; runner.Main(b, runner.ReferenceCatalog(b)) }
