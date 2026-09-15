# TODO:
First, i question whether we need a compose runner at all. We can simply call `docker compose run ` on the host with a small amount of parameter validation
We don't need sql table driven status tracking, as we can always simply call `docker ps` and `docker logs` etc directly, docker is keeping state
We should either remove these extra weight docker management tools, or else better organize the files inside this docker-mcp directory as its becoming heavy.
-KW-2026-09

# Managed Compose profiles

The `compose` tool runs fixed, operator-approved commands inside a configured Compose
service. It knows no application names, config layouts, ETL stages or database names.
Application-specific configuration belongs in gitignored config.json or the application's
own repository. See compose-profile.example.json for a generic configuration.

Install requirements-compose.txt in the host Python environment, configure absolute
Python/helper/Docker paths and rebuild/restart docker-mcp. Each profile uses a `compose`
object; its helper is compose_runner.py. The container must provide Python 3 and Unix
process groups for the timeout wrapper. command_prefix plus the selected operation's
argv is the exact application command; the Compose service's original entrypoint is
replaced. No client-supplied shell, arguments, mounts or working directories are accepted.

Actions: start (run_key, operation), status/logs/cancel (run_key), and optional query
(sql). Each action returns a short-lived bridge job ID; poll get_job_status/tail_job_log
for its JSON result. The durable run_key identifies the actual detached container.
Repeating start returns the same run; a changed recipe is rejected. SQLite reservation
precedes Docker launch, preventing automatic duplication after an ambiguous failure.
A missing reserved container requires operator recovery. MCP restart preserves the
registry and Docker run; the container enforces the configured timeout independently.
Cancel stops that container. Cancelling a bridge job does not stop its container.

Share state_dir between profiles using the same exclusive resource. Only one active or
uncertain run may launch at once; multi-step workflow serialization remains the caller's
responsibility. Retain exited containers until no caller needs their logs or identity.
Do not reset task identities while retaining runs with corresponding keys.

source_paths selects working-directory-relative files/directories to fingerprint;
source_extensions optionally restricts file suffixes. Symlinks escaping the working
directory are rejected. config_file, when present, is fingerprinted separately. Hashes
record launch-time files, not an immutable snapshot; do not modify mounted code during
execution. Configure Compose log rotation: the tool returns only a bounded tail, not
complete logs. Output caps are 2 MiB and 200 log lines. Secret redaction is best-effort;
applications must avoid logging credentials.

## Optional MySQL queries and target checks

Omit database for jobs without SQL. To enable MySQL SELECTs, configure:

```json
{
  "config_file": "/srv/sample/local.yaml",
  "container_config": "/etc/sample/local.yaml",
  "database": {
    "config_keys": ["database", "url"],
    "host": "sample-db",
    "name": "sample",
    "allowed_tables": ["events", "balances"]
  }
}
```

config_keys traverses the YAML to its SQLAlchemy MySQL URL. The host verifies the
approved host/database before job launch. Queries execute inside the Compose network,
rechecking the container config against the same target. Python, SQLAlchemy, PyYAML
and the URL's database driver must exist in that container. No host DB port is needed.
Queries allow one SELECT, only approved tables/schema, no comments, anonymous functions,
locking selects or SELECT INTO. MySQL read-only transactions and a 20-second server
limit apply; results cap at 500 rows/2 MiB. Other SQL engines are not implemented.
These checks constrain this helper, not arbitrary application code; operators remain
responsible for the trusted Compose configuration, mounts and approved operations.

Queue tasks may opt in through replay_profile and call queue_replay. The generic queue
relay pins that profile, checks active attempt ownership, scopes run keys to the task,
and restricts bridge-job polling. It calls the host `compose` tool; worker credentials
never include the host MCP token. The queue knows nothing about application task content.
