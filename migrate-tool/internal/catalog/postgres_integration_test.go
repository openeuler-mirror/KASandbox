package catalog

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestPostgresBuildsQuery(t *testing.T) {
	dsn := os.Getenv("TM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TM_TEST_POSTGRES_DSN is not set")
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Prepare(ctx, "test_postgres_builds_query", postgresBuildsQuery); err != nil {
		t.Fatalf("prepare PostgreSQL Builds query: %v", err)
	}
}
