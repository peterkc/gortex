package main

import (
	"time"

	"github.com/zzet/gortex/internal/persistence"
)

type cliEventRecorder interface {
	AddCLIEvent(time.Time, string, string) error
	Close() error
}
type cliEventReader interface {
	CLIEventsSince(time.Time) ([]persistence.CLIEvent, error)
	CLIEventsBySession(string) ([]persistence.CLIEvent, error)
	Close() error
}
