DROP VIEW platform_manager.account_classification_changes;
DROP TABLE platform_manager.account_amostart_licenses;
DELETE FROM platform_manager.account_refresh_work WHERE component='amostart';
DELETE FROM platform_manager.account_refresh_slots WHERE component='amostart';
ALTER TABLE platform_manager.account_refresh_work DROP CONSTRAINT account_refresh_work_component_check,
    ADD CHECK (component IN ('users','license'));
ALTER TABLE platform_manager.account_refresh_slots DROP CONSTRAINT account_refresh_slots_component_check,
    ADD CHECK (component IN ('users','license','installations'));
