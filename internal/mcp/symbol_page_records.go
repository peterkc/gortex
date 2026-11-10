package mcp

import "context"

type symbolPageRecords interface {
	begin(context.Context, []symbolPageCandidate) (*symbolPageTransaction, error)
	replay(context.Context, int) (symbolPageReplay, bool, error)
	close()
}
