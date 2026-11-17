DROP TABLE platform_manager.event_outbox;
DROP TABLE platform_manager.review_events;
DROP TABLE platform_manager.commands;
DROP FUNCTION platform_manager.require_command_outcome();
DROP TABLE platform_manager.snapshot_items;
DROP TABLE platform_manager.snapshots;
DROP VIEW platform_manager.installations;
DROP VIEW platform_manager.approved_pairs;
DROP SCHEMA platform_manager;

DROP TRIGGER project_episode_transition ON platform_lifecycle.history;
DROP FUNCTION platform_lifecycle.project_episode_transition();
ALTER TABLE platform_lifecycle.episodes
    DROP COLUMN reviewed_at, DROP COLUMN reviewed_by, DROP COLUMN version, DROP COLUMN last_lifecycle_event;
ALTER TABLE platform_lifecycle.products DROP COLUMN line, DROP COLUMN display_name;
