package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
func dispatch(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("measure -config PATH -config-pin SHA256 or internal worker command required")
	}
	switch args[0] {
	case "measure":
		return measure(args[1:])

	case "worker":
		fs := flag.NewFlagSet("worker", flag.ContinueOnError)
		cfgPath := fs.String("config", "", "frozen config")
		pin := fs.String("config-pin", "", "SHA256")
		task := fs.String("task", "", "task")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected worker arguments")
		}
		cfg, err := readConfig(*cfgPath, *pin)
		if err != nil {
			return err
		}
		return work(context.Background(), cfg, *task)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
