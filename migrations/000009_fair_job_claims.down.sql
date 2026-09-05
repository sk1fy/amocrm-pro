DROP INDEX IF EXISTS jobs_installation_processing_idx;
DROP INDEX IF EXISTS jobs_installation_ready_idx;
DROP TRIGGER IF EXISTS integrations_create_job_queue_lane ON integrations;
DROP FUNCTION IF EXISTS create_integration_job_queue_lane();
DROP TABLE IF EXISTS job_queue_lanes;
