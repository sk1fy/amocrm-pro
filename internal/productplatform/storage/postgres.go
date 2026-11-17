package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const Database = "amocrm_platform"

type Role string

const (
	Owner  Role = "platform_owner"
	Access Role = "platform_access"
	Jobs   Role = "platform_jobs"
)

func verifyIdentity(ctx context.Context, conn *pgx.Conn, role Role) error {
	var valid bool
	err := conn.QueryRow(ctx, `SELECT current_database() = $1 AND session_user = $2 AND current_user = $2`, Database, string(role)).Scan(&valid)
	if err != nil || !valid {
		return errors.New("PostgreSQL database or role verification failed")
	}
	return nil
}

func Open(ctx context.Context, dsn string, role Role, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection configuration")
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MinIdleConns = 0
	cfg.ConnConfig.RuntimeParams["application_name"] = string(role)
	cfg.ConnConfig.RuntimeParams["search_path"] = "pg_catalog"
	if role == Owner {
		cfg.ConnConfig.RuntimeParams["search_path"] = "public"
	}
	if role == Access {
		cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return verifyIdentity(ctx, conn, role)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("could not create PostgreSQL pool")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("could not connect to PostgreSQL with the configured role")
	}
	return pool, nil
}

func CheckAccess(ctx context.Context, pool *pgxpool.Pool) error {
	var ready bool
	err := pool.QueryRow(ctx, `SELECT ready FROM platform_access.runtime_probe`).Scan(&ready)
	if err != nil || !ready {
		return errors.New("access database is unavailable")
	}
	return nil
}
