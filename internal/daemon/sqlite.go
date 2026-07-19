package daemon

import (
	"context"
	"log/slog"

	"github.com/priyavratuniyal/tuskbase/internal/adapters/sqlite"
	"github.com/priyavratuniyal/tuskbase/internal/ports"
	"github.com/priyavratuniyal/tuskbase/internal/search"
)

// SQLiteStoreFactory powers Local Basic. It is a factory so Local Shared can use
// Postgres without changing daemon wiring.
type SQLiteStoreFactory struct {
	Path      string
	Embedding ports.EmbeddingProvider
	Logger    *slog.Logger
}

func (f SQLiteStoreFactory) Open(ctx context.Context) (StoreBundle, error) {
	store, err := sqlite.Open(ctx, f.Path)
	if err != nil {
		return StoreBundle{}, err
	}
	idx := search.NewHybridIndex(store, store, f.Embedding, f.Logger)
	return StoreBundle{Store: store, Search: idx, Close: store.Close, Name: "sqlite"}, nil
}
