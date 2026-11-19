DROP TRIGGER schedule_installation_deal ON platform_lifecycle.history;
DROP FUNCTION platform_manager.schedule_installation_deal();
DELETE FROM platform_jobs.jobs WHERE kind='installation.deal';
DROP TABLE platform_manager.installation_deal_retries;
DROP TABLE platform_manager.installation_deals;
DROP TABLE platform_manager.installation_contacts;
