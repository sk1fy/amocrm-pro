ALTER TABLE command_receipts
    DROP CONSTRAINT command_receipts_actor_id_check,
    ADD CONSTRAINT command_receipts_actor_id_nonnegative CHECK (actor_id >= 0);
