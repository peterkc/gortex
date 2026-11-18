package review

import (
	"time"

	"github.com/zzet/gortex/internal/persistence"
)

type suppressionRecords interface {
	LoadSuppression(string, string) (persistence.SuppressionEntry, bool)
	UpsertSuppression(string, persistence.SuppressionEntry) error
	BumpSuppressionHit(string, string, time.Time) error
	LoadSuppressions(string) ([]persistence.SuppressionEntry, error)
	DeleteSuppression(string, string) error
}
