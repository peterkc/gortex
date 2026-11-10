package store_sqlite

import (
	"context"
	"reflect"
	"testing"
)

func TestConsumerStoreAdaptersKeepNativeHandles(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, ok := store.ViewScopedAnalysisStore().(*Store)
	if !ok || got.storeCore != store.storeCore || got.viewGen != store.viewGen || !got.analysisViewScoped || got.ownsCore {
		t.Fatalf("view-scoped adapter changed native identity: %#v", got)
	}
	var absent *Store
	if absent.ViewScopedAnalysisStore() != nil {
		t.Fatal("nil native handle became non-nil interface")
	}
	ctx := context.Background()
	texts := []string{"ExampleName other"}
	wantTokens, err := SymbolFTSTokenize(ctx, texts)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := store.TokenizeSymbolFTS(ctx, texts)
	if err != nil || !reflect.DeepEqual(tokens, wantTokens) {
		t.Fatalf("tokenizer changed: %v, %v", tokens, err)
	}
	wantRoute, wantFound, wantErr := store.Catalog().GetCheckoutRoute(ctx, "missing")
	route, found, err := store.GetCheckoutRoute(ctx, "missing")
	if route != wantRoute || found != wantFound || err != wantErr {
		t.Fatalf("route adapter changed: %v, %v, %v", route, found, err)
	}
	wantErr = store.Catalog().WithdrawProducer(ctx, 0, "source.snapshot", "missing")
	err = store.WithdrawProducer(ctx, 0, "source.snapshot", "missing")
	if (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error() {
		t.Fatalf("withdrawal adapter changed: %v, want %v", err, wantErr)
	}
}
