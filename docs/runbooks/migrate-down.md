# Destructive migration rollback

`migrate down` reverts every applied migration. It removes the application
schema and its data. It is not a normal deployment or recovery operation.

## Preconditions

Before confirming a rollback:

1. Verify the exact Compose project and PostgreSQL target. Never infer the
   target from terminal history.
2. Stop API and worker processes that can write to the database.
3. Create a restorable backup and verify where it is stored.
4. Record the current migration list and confirm that full schema removal is
   the intended operation.
5. Plan recovery: either restore the backup or apply migrations again with
   `make migrate`.

Do not put database credentials, backup contents, or production data in the
confirmation value, logs, Issues, or checkpoint evidence.

## Invocation

The migrate binary refuses `down` unless the exact one-operation confirmation
is supplied. From the repository root, after completing the prerequisites:

```sh
make migrate-down MIGRATION_DOWN_CONFIRM=revert-all-migrations
```

The Make target passes the confirmation only to that ephemeral migrate
container. The default Compose environment does not persist or preconfigure it.

## Verification and recovery

Confirm that the migrate command completed against the intended target. If the
rollback was part of an approved reset, apply the current schema with:

```sh
make migrate
```

Then verify migration status, application readiness, and expected data before
restarting writers. If data must be recovered, keep writers stopped, restore
the verified backup using the environment's approved PostgreSQL procedure, and
validate migrations and readiness before resuming traffic.
