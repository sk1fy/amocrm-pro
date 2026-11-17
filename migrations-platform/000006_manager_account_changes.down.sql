DROP TABLE platform_manager.account_event_outbox;
DROP TABLE platform_manager.account_change_events;
DROP TABLE platform_manager.account_profiles;

DELETE FROM platform_manager.commands WHERE kind NOT IN ('installation.review', 'account.refresh');

ALTER TABLE platform_manager.commands
    DROP CONSTRAINT commands_target_shape,
    DROP CONSTRAINT commands_outcome_shape,
    DROP COLUMN resource_id,
    DROP COLUMN product_id,
    ADD CONSTRAINT commands_target_shape CHECK (
        (kind = 'installation.review' AND episode_id IS NOT NULL AND expected_version IS NOT NULL AND account_id IS NULL)
        OR (kind = 'account.refresh' AND account_id IS NOT NULL AND episode_id IS NULL AND expected_version IS NULL)),
    ADD CONSTRAINT commands_outcome_shape CHECK (
        (status IS NULL AND response IS NULL AND etag IS NULL)
        OR (status IS NOT NULL AND response IS NOT NULL AND octet_length(response) > 0 AND (
            (kind = 'installation.review' AND status IN (200, 409) AND (etag IS NOT NULL) = (status = 200))
            OR (kind = 'account.refresh' AND status = 202 AND etag IS NULL)
        )));
