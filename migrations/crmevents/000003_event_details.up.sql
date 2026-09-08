-- Optional Events API contact reference belongs to the CRM Events owner.
-- Zero means absent/unknown; no foreign key points into another owner's data.
ALTER TABLE crm_events
    ADD COLUMN linked_talk_contact_id bigint NOT NULL DEFAULT 0
    CHECK (linked_talk_contact_id >= 0);
