package savings

import (
	"time"

	"github.com/zzet/gortex/internal/persistence"
)

type ledgerRecords interface {
	AddSavingsObservations([]persistence.SavingsEvent) error
	SavingsTotals() (map[string]persistence.SavingsTotalsRow, time.Time, time.Time, error)
	SavingsEventsSince(time.Time) ([]persistence.SavingsEvent, error)
	SavingsLegacyImportDone() bool
	ImportLegacySavings(map[string]persistence.SavingsTotalsRow, time.Time, time.Time, []persistence.SavingsEvent) error
	SavingsToolTotals(time.Time) ([]persistence.SavingsToolRow, error)
	SavingsModelTotals(time.Time) ([]persistence.SavingsDimRow, error)
	SavingsClientTotals(time.Time) ([]persistence.SavingsDimRow, error)
	ResetSavings() error
	Close() error
}
