package postgres_test

import (
	"context"
	"os"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Runs shared between machines: the lease is a row on the database clock, and a runner that lost its lease
// is fenced at its next write. (Needs a database, so this example is compiled, not run.)
func ExampleNew() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		panic(err)
	}
	defer pool.Close()
	db, err := postgres.New(ctx, pool) // creates the tables if needed
	if err != nil {
		panic(err)
	}
	r, err := agentsafe.New(&agentsafe.ScriptedModel{Final: "done"}, &agentsafe.Journal{Store: db.Run("payouts-2026-10-06")})
	if err != nil {
		panic(err)
	}
	_, _ = r.Start(ctx, "sys", "task")
}
