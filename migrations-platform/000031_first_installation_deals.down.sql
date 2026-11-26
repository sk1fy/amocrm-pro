DROP TRIGGER schedule_installation_deal ON platform_lifecycle.history;

CREATE TRIGGER schedule_installation_deal AFTER INSERT ON platform_lifecycle.history
FOR EACH ROW WHEN (NEW.kind IN ('installed', 'reinstalled'))
EXECUTE FUNCTION platform_manager.schedule_installation_deal();
