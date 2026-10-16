-- R07 diagnostic runtime package; evidence remains installation-generation scoped.
ALTER TABLE task_control_runtime_descriptors DROP CONSTRAINT task_control_runtime_descriptors_runtime_version_check;
ALTER TABLE task_control_runtime_descriptors ADD CONSTRAINT task_control_runtime_descriptors_runtime_version_check CHECK(runtime_version IN ('1.1.4','1.2.0','1.2.1','1.2.2'));
