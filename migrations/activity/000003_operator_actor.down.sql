ALTER TABLE command_receipts
    DROP CONSTRAINT command_receipts_actor_id_nonnegative,
    ADD CONSTRAINT command_receipts_actor_id_check CHECK (actor_id > 0);
