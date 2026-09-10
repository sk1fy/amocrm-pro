DROP TRIGGER IF EXISTS event_inbox_reject_tombstoned_command ON event_inbox;
DROP FUNCTION IF EXISTS event_inbox_reject_tombstoned_command();
DROP INDEX IF EXISTS event_operations_technical_history;
DROP INDEX IF EXISTS event_inbox_created;
DROP INDEX IF EXISTS event_command_tombstones_cleanup;
DROP TABLE IF EXISTS event_command_tombstones;
