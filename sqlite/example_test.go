package sqlite_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/sqlite"
)

// Many runs in one SQLite file; each run is fenced (one writer per event) and leased (one runner at a time).
func ExampleOpen() {
	dir, _ := os.MkdirTemp("", "agentsafe-sqlite")
	defer func() { _ = os.RemoveAll(dir) }()
	db, err := sqlite.Open(filepath.Join(dir, "runs.db"))
	if err != nil {
		panic(err)
	}
	defer func() { _ = db.Close() }()

	r, err := agentsafe.New(&agentsafe.ScriptedModel{Final: "nothing to do"},
		&agentsafe.Journal{Store: db.Run("payouts-2026-10-06")})
	if err != nil {
		panic(err)
	}
	st, err := r.Start(context.Background(), "sys", "task")
	fmt.Println(st.Status, err)
	// Output:
	// finished <nil>
}
