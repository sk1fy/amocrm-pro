-- Never relabel deployed package evidence on rollback.
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM task_control_runtime_descriptors WHERE runtime_version='1.2.2') THEN
  RAISE EXCEPTION 'Runtime 1.2.2 descriptors exist: withdraw or replace the deployment evidence before schema rollback';
 END IF;
END $$;
ALTER TABLE task_control_runtime_descriptors DROP CONSTRAINT task_control_runtime_descriptors_runtime_version_check;
ALTER TABLE task_control_runtime_descriptors ADD CONSTRAINT task_control_runtime_descriptors_runtime_version_check CHECK(runtime_version IN ('1.1.4','1.2.0','1.2.1'));
