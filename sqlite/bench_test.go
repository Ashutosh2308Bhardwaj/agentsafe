package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/sqlite"
)

// BenchmarkAppend is one durable event append to a SQLite run (one transaction, WAL, synchronous=FULL and fullfsync as
// configured by Open): the per-transition durability cost when the log lives in SQLite.
func BenchmarkAppend(b *testing.B) {
	db, err := sqlite.Open(filepath.Join(b.TempDir(), "runs.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	log := &agentsafe.Journal{Store: db.Run("bench")}
	ev := agentsafe.Event{V: agentsafe.FormatVersion, Type: agentsafe.EvToolStarted, CallID: "call_1", Tool: "pay",
		Args: `{"invoice_id":"INV-0001","amount":"4200.50"}`, Key: "00000000000000000000000000000001"}
	ctx := context.Background()
	b.ReportAllocs()
	for range b.N {
		if err := log.Append(ctx, ev); err != nil {
			b.Fatal(err)
		}
	}
}
