# Queue profiles

Copy [profiles.json](profiles.json) to your host, for example
`~/.config/agent-manager/task-queue-profiles.json`. Edit repository/instruction paths
and task settings; all paths inside the JSON refer to locations **inside the container**.

Mount the file read-only and set these variables on the agent-manager service:

```yaml
volumes:
  - ${HOME}/.config/agent-manager/task-queue-profiles.json:/etc/agent-manager/task-queue-profiles.json:ro
environment:
  AM_TASK_QUEUE_PROFILES: /etc/agent-manager/task-queue-profiles.json
  <DB_ENV_VAR>: "${DB_ENV_VAR}"
```

- `AM_TASK_QUEUE_PROFILES` points to the JSON file inside the container.
- `<DB_ENV_VAR>` is defined in the profile json by `database_env` and it names
  the variable holding that profile's database connection string.
  Supply its value through your host environment or secret configuration, using
  Go MySQL DSN syntax: `user:password@tcp(host:3306)/database`. Keep credentials out
  of the JSON and Git. Recreate the container after changing service environment variables.
- The profile name (`sample`) is the SQL `queue_id` and UI profile selection;
  a task key (`report`) is the SQL `task_type`. Defaults populate UI controls.

## Render and load tasks

Task entries may declare `inputs` and `outputs` as file-path arrays. Use `{{topic}}`
(or another parameter name) in Markdown and paths. `upstream:reports/README.md`
requires that exact accepted file from the immediate predecessor; plain paths refer
to the worker's working directory. Output paths must be relative and have no prefix.

Use **Load tasks** on a queue's Tasks panel to paste/upload [batch.json](batch.json),
preview assignments, then load them. This works while the controller is stopped.
An active controller may dispatch newly loaded tasks immediately.

Equivalent operator CLI commands inside the container:

```sh
python -m agent_manager.orchestration render --profile sample --file batch.json
python -m agent_manager.orchestration enqueue --profile sample --file batch.json
```

`batch_key` plus `workflow_id` identifies a repeat-safe batch within the queue.
Reusing it with changed content conflicts. `depends_on` names an earlier batch task
key; `depends_on_id` references an existing task in the same queue/workflow.
Missing/pending files appear in preview notes and are checked at execution time.
Preview is not a frozen execution version: new attempts resolve current definitions;
retries retain their saved assignment.

Completion requires every declared output to exist as a contained regular file and
be created or rewritten during the attempt. The controller archives those files even
if the worker omits them from artifact_paths. These checks verify files, not correctness;
human review remains necessary. Blocked/failed results may submit partial evidence.

Database connection variables accept either a MySQL URL
(`mysql://USER:PASSWORD@HOST:3306/DATABASE`) or a native MySQL DSN
(`USER:PASSWORD@tcp(HOST:3306)/DATABASE`). Percent-encode special characters in URL
credentials. A missing engine prefix means MySQL; other explicit engine prefixes
are rejected until supported. URL query parameters use Go MySQL driver option names.
