-- Keep the obsolete subscription column until a later contract migration:
-- the old process can still be running between migrate and deploy.
ALTER TABLE activity_problem_sources ALTER COLUMN requested_until SET DEFAULT now();
ALTER TABLE activity_problem_sources ADD COLUMN batch_records INTEGER NOT NULL DEFAULT 0 CHECK(batch_records>=0);
