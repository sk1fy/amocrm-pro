DROP TRIGGER queue_title_on_reauthorization ON platform_lifecycle.history;
DROP TRIGGER queue_title_on_deal ON platform_manager.installation_deals;
DROP FUNCTION platform_manager.queue_installation_deal_title();
DELETE FROM platform_jobs.jobs WHERE kind='installation.deal.title';
