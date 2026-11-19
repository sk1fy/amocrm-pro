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

func CheckManager(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		SELECT a.account_id,p.version,c.product_id,c.resource_id,pt.version,
		       n.version,nr.version,t.version,ad.audit_date,term.entitlement_version,
		       catalog.catalog_version,catalog.catalog_policy_version,catalog.tariff_version,revision.version,
		       history.source_version,tariff.evidence,verified.rule,active.own_until,
		       event.source,event.id,event.kind,event.occurred_at,event.recorded_at,event.account_id,event.product_id,event.episode_id,
		       snapshot.route,snapshot.account_id,snapshot.query_hash,snapshot.scope_hash,snapshot.total,item.ordinal,item.source,item.event_id
		FROM platform_manager.visible_accounts a
		CROSS JOIN platform_manager.installation_directory installation
		CROSS JOIN platform_manager.installation_contacts contact
		CROSS JOIN platform_manager.installation_deals deal
		CROSS JOIN platform_manager.installation_deal_retries retry
		CROSS JOIN platform_manager.account_profiles p
		CROSS JOIN platform_manager.commands c
		CROSS JOIN platform_manager.account_portraits pt
		CROSS JOIN platform_manager.portrait_niches n
		CROSS JOIN platform_manager.portrait_niche_revision nr
		CROSS JOIN platform_manager.portrait_template t
		CROSS JOIN platform_manager.portrait_audit_dates ad
		CROSS JOIN platform_lifecycle.terms term
		CROSS JOIN platform_lifecycle.products catalog
		CROSS JOIN platform_manager.product_catalog_revision revision
		CROSS JOIN platform_lifecycle.product_trial_revisions history
		CROSS JOIN platform_lifecycle.verified_tariff_versions tariff
		CROSS JOIN platform_lifecycle.verified_trial_policies verified
		CROSS JOIN platform_manager.product_access active
		CROSS JOIN platform_manager.business_events event
		CROSS JOIN platform_manager.feed_snapshots snapshot
		CROSS JOIN platform_manager.feed_snapshot_items item
        CROSS JOIN platform_company.source_control company_control
        CROSS JOIN platform_company.product_workflow_heads workflow_head
		LIMIT 0`); err != nil {
		return errors.New("manager read schema is unavailable")
	}
	var allowed bool
	err := pool.QueryRow(ctx, `
		SELECT has_table_privilege('platform_company.generations','SELECT')
           AND has_table_privilege('platform_company.product_workflow_revisions','INSERT')
           AND has_column_privilege('platform_company.product_workflow_heads','version','UPDATE')
           AND has_function_privilege('platform_company.register_source(bigint,text)','EXECUTE')
           AND has_function_privilege('platform_company.enqueue_refresh(uuid,bigint,text,uuid,text)','EXECUTE')
           AND has_table_privilege('platform_manager.business_events','SELECT')
		   AND has_table_privilege('platform_manager.feed_snapshots','SELECT')
		   AND has_table_privilege('platform_manager.feed_snapshots','INSERT')
		   AND has_table_privilege('platform_manager.feed_snapshots','DELETE')
		   AND has_table_privilege('platform_manager.feed_snapshot_items','SELECT')
		   AND has_table_privilege('platform_manager.feed_snapshot_items','INSERT')
		   AND has_column_privilege('platform_manager.account_snapshot_admission','marker','UPDATE')
		   AND has_table_privilege('platform_manager.account_change_events', 'INSERT')
		   AND has_table_privilege('platform_manager.account_event_outbox', 'INSERT')
		   AND has_table_privilege('platform_lifecycle.product_trial_revisions','INSERT')
		   AND has_function_privilege('platform_manager.apply_catalog_policy(uuid,bigint)','EXECUTE')
		   AND has_function_privilege('platform_manager.adopt_trial_evidence(uuid)','EXECUTE')
		   AND (SELECT bool_and(has_column_privilege('platform_lifecycle.products',field,'UPDATE'))
		        FROM unnest(ARRAY['catalog_version','catalog_policy_version','display_name','line','availability','links','tariff_version']) field)
		   AND (SELECT bool_and(
		            has_column_privilege('platform_lifecycle.terms', field, 'INSERT')
		        AND has_column_privilege('platform_lifecycle.terms', field, 'UPDATE'))
		        FROM unnest(ARRAY['valid_until','source','reason','entitlement_version']) AS field)
	`).Scan(&allowed)
	if err != nil || !allowed {
		return errors.New("manager mutation grants are unavailable")
	}
	return nil
}
