DROP FUNCTION platform_manager.enqueue_account_refresh(UUID, TEXT, UUID);
DROP INDEX platform_jobs.account_refresh_active;
DROP TABLE platform_manager.account_refresh_work;
DROP TABLE platform_manager.account_refresh_slots;
DELETE FROM platform_jobs.jobs WHERE kind = 'accounts.refresh';
DELETE FROM platform_manager.commands WHERE kind = 'account.refresh';

ALTER TABLE platform_manager.commands
    DROP CONSTRAINT commands_target_shape,
    DROP CONSTRAINT commands_outcome_shape,
    DROP COLUMN account_id,
    ALTER COLUMN episode_id SET NOT NULL,
    ALTER COLUMN expected_version SET NOT NULL,
    ADD CONSTRAINT commands_kind_check CHECK (kind = 'installation.review'),
    ADD CONSTRAINT commands_check CHECK ((status IS NULL AND response IS NULL AND etag IS NULL)
        OR (status IS NOT NULL AND status IN (200, 409) AND octet_length(response) > 0
            AND response IS NOT NULL AND (etag IS NOT NULL) = (status = 200)));

DROP TABLE platform_manager.account_snapshot_admission;
DROP TABLE platform_manager.account_snapshot_items;
DROP TABLE platform_manager.account_snapshots;
DROP TABLE platform_manager.account_rosters;
DROP TABLE platform_manager.account_domains;
DROP TABLE platform_manager.account_identities;
DROP VIEW platform_manager.visible_accounts;
DROP VIEW platform_manager.account_memberships;
ALTER TABLE platform_jobs.jobs DROP COLUMN failure_code, DROP COLUMN updated_at;
