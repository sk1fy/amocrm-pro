package migrations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const advisoryLockKey int64 = 6_173_375_269_556_812_923

var migrationNamePattern = regexp.MustCompile(`^([0-9]{6})_[a-z0-9_]+\.up\.sql$`)

type Runner struct {
	pool *pgxpool.Pool
	dir  string
}

type migration struct {
	name         string
	downName     string
	up           []byte
	down         []byte
	checksum     [sha256.Size]byte
	downChecksum [sha256.Size]byte
}

type appliedMigration struct {
	checksum     []byte
	downChecksum []byte
}

func New(pool *pgxpool.Pool, dir string) *Runner {
	return &Runner{pool: pool, dir: dir}
}

func (r *Runner) Up(ctx context.Context) error {
	migrations, err := r.load()
	if err != nil {
		return err
	}
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer unlock(conn, ctx)

	if err := ensureMetadata(ctx, conn); err != nil {
		return err
	}
	applied, err := readApplied(ctx, conn)
	if err != nil {
		return err
	}
	if err := preflight(migrations, applied); err != nil {
		return err
	}

	for _, item := range migrations {
		if _, exists := applied[item.name]; exists {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", item.name, err)
		}
		if _, err := tx.Exec(ctx, string(item.up)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", item.name, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO schema_migrations (version, checksum, down_checksum)
			VALUES ($1, $2, $3)`, item.name, item.checksum[:], item.downChecksum[:]); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", item.name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", item.name, err)
		}
	}
	return nil
}

func (r *Runner) Down(ctx context.Context) error {
	migrations, err := r.load()
	if err != nil {
		return err
	}
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer unlock(conn, ctx)

	applied, err := readApplied(ctx, conn)
	if err != nil {
		if isUndefinedTable(err) {
			return nil
		}
		return err
	}
	if err := preflight(migrations, applied); err != nil {
		return err
	}
	for index := len(migrations) - 1; index >= 0; index-- {
		item := migrations[index]
		if _, exists := applied[item.name]; !exists {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin down migration %s: %w", item.downName, err)
		}
		if _, err := tx.Exec(ctx, string(item.down)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply down migration %s: %w", item.downName, err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM schema_migrations WHERE version = $1", item.name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("remove migration version %s: %w", item.name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit down migration %s: %w", item.downName, err)
		}
	}
	return nil
}

func (r *Runner) load() ([]migration, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations directory: %w", err)
	}
	names := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names[entry.Name()] = struct{}{}
		}
	}

	items := make([]migration, 0)
	versions := make(map[string]string)
	for name := range names {
		matches := migrationNamePattern.FindStringSubmatch(name)
		if len(matches) == 0 {
			if strings.HasSuffix(name, ".up.sql") {
				return nil, fmt.Errorf("migration %s does not match NNNNNN_name.up.sql", name)
			}
			continue
		}
		if previous, exists := versions[matches[1]]; exists {
			return nil, fmt.Errorf("migration numeric version %s is used by both %s and %s", matches[1], previous, name)
		}
		versions[matches[1]] = name
		downName := strings.TrimSuffix(name, ".up.sql") + ".down.sql"
		if _, exists := names[downName]; !exists {
			return nil, fmt.Errorf("migration %s has no paired %s", name, downName)
		}
		up, err := os.ReadFile(filepath.Join(r.dir, name))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		down, err := os.ReadFile(filepath.Join(r.dir, downName))
		if err != nil {
			return nil, fmt.Errorf("read down migration %s: %w", downName, err)
		}
		items = append(items, migration{
			name: name, downName: downName, up: up, down: down,
			checksum: sha256.Sum256(up), downChecksum: sha256.Sum256(down),
		})
	}
	for name := range names {
		if !strings.HasSuffix(name, ".down.sql") {
			continue
		}
		upName := strings.TrimSuffix(name, ".down.sql") + ".up.sql"
		if _, exists := names[upName]; !exists {
			return nil, fmt.Errorf("down migration %s has no paired %s", name, upName)
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no migration pairs found in %s", r.dir)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	return items, nil
}

func ensureMetadata(ctx context.Context, conn *pgxpool.Conn) error {
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			checksum BYTEA NOT NULL,
			down_checksum BYTEA NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	if _, err := conn.Exec(ctx, `
		ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum BYTEA;
		ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS down_checksum BYTEA`); err != nil {
		return fmt.Errorf("ensure migration checksum columns: %w", err)
	}
	return nil
}

func readApplied(ctx context.Context, conn *pgxpool.Conn) (map[string]appliedMigration, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum, down_checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("list applied migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[string]appliedMigration)
	for rows.Next() {
		var version string
		var record appliedMigration
		if err := rows.Scan(&version, &record.checksum, &record.downChecksum); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[version] = record
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return applied, nil
}

func preflight(local []migration, applied map[string]appliedMigration) error {
	localByName := make(map[string]migration, len(local))
	for _, item := range local {
		localByName[item.name] = item
	}
	for version := range applied {
		if _, exists := localByName[version]; !exists {
			return fmt.Errorf("applied migration %s is missing from the artifact", version)
		}
	}
	pendingSeen := false
	for _, item := range local {
		record, exists := applied[item.name]
		if !exists {
			pendingSeen = true
			continue
		}
		if pendingSeen {
			return fmt.Errorf("migration history is divergent: %s is applied after a pending earlier version", item.name)
		}
		if len(record.checksum) != sha256.Size || len(record.downChecksum) != sha256.Size {
			return fmt.Errorf("migration %s was applied without complete checksums; explicit repair or pre-release reset is required", item.name)
		}
		if !bytes.Equal(record.checksum, item.checksum[:]) || !bytes.Equal(record.downChecksum, item.downChecksum[:]) {
			return fmt.Errorf("migration %s checksum differs from applied history", item.name)
		}
	}
	return nil
}

func unlock(conn *pgxpool.Conn, parent context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)
	defer cancel()
	_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)
}

func isUndefinedTable(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "42P01"
}
