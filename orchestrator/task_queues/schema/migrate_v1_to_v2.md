# Explicit migration from schema 1 to 2

Fresh installations use `002.sql` through `init-schema`. `001.sql` is historical.
The installer refuses an existing version 1 namespace; it does not alter it.
No deployed database has been migrated by this change.

For a pre-pilot installation with no history to retain, use a fresh table prefix
and initialize version 2. Leave the original namespace intact until inspected.

For an installation with existing history, schedule an explicit maintenance migration:

1. Pause and drain every controller using the namespace. Confirm no attempt has
   status `claimed`, `running` or `submitted`, then stop every controller. Prevent
   producer writes for the migration. Back up SQL tables, state and artifacts.
2. Add nullable `backend_id VARCHAR(64)` to the attempts table. Populate it by
   joining attempts to the existing queues table on `queue_id`, copying the queue's
   `backend_id`. Verify every attempt has a non-null value, then make it NOT NULL.
   Preserve the original backend secret; fingerprints are not replacement secrets.
3. Inspect `information_schema.KEY_COLUMN_USAGE` for the tasks table's foreign key
   referencing the queues table. Drop that specific foreign key with `ALTER TABLE`.
   Preserve the predecessor and attempt-to-task foreign keys and all indexes.
4. Drop the queues table after the verified backup. Its old pause/limit settings
   are no longer shared; record them beforehand if useful for operator reference.
5. Insert schema version 2 into the version table. Verify the resulting table
   definitions against `002.sql`, ignoring historical constraint names and the
   retained version 1 row. The runtime checks `MAX(version)`.
6. Configure each controller's initial limit (start at 1). Start paused if inspecting
   state through the controller API, then explicitly resume only intended controllers.
   Verify task counts, completed results, attempts and artifacts remain accessible.

MySQL DDL is not one rollbackable transaction. Inspect the schema after any failure
and resume deliberately; do not blindly rerun an ADD/DROP sequence. Restore the
backup if validation fails before restarting writers. This migration procedure is
an operator checklist, not an automatically executed or tested migration script.
