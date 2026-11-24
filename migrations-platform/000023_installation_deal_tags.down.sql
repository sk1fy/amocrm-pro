DROP TRIGGER queue_tags_on_reauthorization ON platform_lifecycle.history;
DROP TRIGGER queue_tags_on_deal ON platform_manager.installation_deals;
DROP FUNCTION platform_manager.queue_installation_deal_tags();
DELETE FROM platform_jobs.jobs WHERE kind='installation.deal.tags';
