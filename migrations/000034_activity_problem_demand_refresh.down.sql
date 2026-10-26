ALTER TABLE activity_problem_sources DROP COLUMN batch_records;
ALTER TABLE activity_problem_sources ALTER COLUMN requested_until DROP DEFAULT;
