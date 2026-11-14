package mcp

import (
	"time"

	"github.com/zzet/gortex/internal/persistence"
)

type memoryRecords interface {
	PrefixSymbolIDs(string, persistence.SymbolIDRewriter) (int, error)
	LoadMemoriesRows(string) ([]persistence.MemoryEntry, error)
	DeleteMemory(string, string) error
	UpsertMemory(string, persistence.MemoryEntry) error
	TrimMemories(string, int) error
}
type noteRecords interface {
	LoadNotesRows(string) ([]persistence.NoteEntry, error)
	DeleteNote(string, string) error
	UpsertNote(string, persistence.NoteEntry) error
	TrimNotes(string, int) error
}
type notebookRecords interface {
	UpsertNotebook(string, persistence.NotebookRow) error
	GetNotebookRow(string, string) (persistence.NotebookRow, bool)
	DeleteNotebook(string, string) error
	LoadNotebookRows(string) ([]persistence.NotebookRow, error)
	NotebookPrune(string, time.Time) error
}
type promotionRecords interface {
	BumpSessionEpoch(string) (int64, error)
	LoadPromotedTools(string) ([]persistence.PromotedToolEntry, error)
	DeletePromotedTool(string, string) error
	UpsertPromotedTool(string, persistence.PromotedToolEntry) error
}
type scopeRecords interface {
	UpsertScope(persistence.ScopeRow) error
	DeleteScope(string) error
}
