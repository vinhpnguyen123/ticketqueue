package db

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultMaxConns is the pool size when POOL_MAX_CONNS is unset.
//
// Week 6 measured this as the application's real ceiling on a laptop:
// db_pool_in_use sat pinned at 10 from 1,000 VUs upward while the payment call
// stayed at 5ms, and the 2,000-VU run accumulated ~15,000 seconds of acquire
// wait. It is configurable now so that number can be measured rather than
// argued about — see docs/load-test-results.md.
const DefaultMaxConns = 10

// NewPool creates a pgx connection pool and verifies connectivity with a Ping.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	cfg.MaxConns = DefaultMaxConns
	if raw := os.Getenv("POOL_MAX_CONNS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("POOL_MAX_CONNS must be a positive integer, got %q", raw)
		}
		cfg.MaxConns = int32(n)
	}

	// ---------------------------------------------------------------------
	// THE PGBOUNCER TRAP. Read this before changing anything below.
	//
	// pgx v5 prepares every statement by default and caches it against the
	// connection it was prepared on. PgBouncer in TRANSACTION mode hands out a
	// different server connection per transaction, so the connection that runs
	// a query is usually not the one that prepared it. Postgres then answers:
	//
	//     ERROR: prepared statement "lrupsc_1_0" does not exist (SQLSTATE 26000)
	//
	// What makes this genuinely dangerous is the failure pattern, not the
	// error. With one client and one transaction at a time, PgBouncer keeps
	// handing back the same server connection and everything passes — a smoke
	// test, an integration suite, a manual click-through. The moment real
	// concurrency spreads transactions across the server pool, statements start
	// missing. So it looks like it works, ships, and then fails under exactly
	// the load the system exists to handle.
	//
	// SimpleProtocol sends the SQL text with its arguments interpolated on
	// every execution, so nothing is stored on the server between round trips
	// and any connection can serve any query. The costs, stated honestly:
	// one fewer round trip is lost, the server re-plans each statement, and
	// arguments are escaped by pgx rather than sent out-of-band. pgx's escaping
	// is what makes the last one safe, and it is the documented, supported
	// configuration for transaction pooling.
	//
	// Driven by PGBOUNCER rather than sniffing the port, because ":6432" is a
	// convention, not a fact — an RDS Proxy or an ECS service listens wherever
	// it was told to. An explicit switch cannot be fooled by a port number.
	// ---------------------------------------------------------------------
	viaPgBouncer := os.Getenv("PGBOUNCER") == "true"
	if viaPgBouncer {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres at startup (is the database up and DATABASE_URL correct?): %w", err)
	}

	// Logged at startup because the two modes fail in completely different
	// ways, and the first question when a "prepared statement does not exist"
	// appears is which mode was actually active.
	slog.Info("postgres pool ready",
		"max_conns", cfg.MaxConns,
		"pgbouncer", viaPgBouncer,
		"query_exec_mode", queryExecModeName(cfg.ConnConfig.DefaultQueryExecMode))

	return pool, nil
}

func queryExecModeName(m pgx.QueryExecMode) string {
	switch m {
	case pgx.QueryExecModeSimpleProtocol:
		return "simple_protocol"
	case pgx.QueryExecModeExec:
		return "exec"
	case pgx.QueryExecModeDescribeExec:
		return "describe_exec"
	case pgx.QueryExecModeCacheDescribe:
		return "cache_describe"
	default:
		return "cache_statement"
	}
}
