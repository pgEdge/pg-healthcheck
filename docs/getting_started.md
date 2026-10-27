# Installation

pg-healthcheck is a single statically linked binary. The following
sections cover building from source and the prerequisites required
before running any checks.

## Exit Codes

pg-healthcheck uses structured exit codes so it integrates directly
with CI/CD pipelines, alerting systems, and monitoring scripts:

| Exit code | Meaning |
|-----------|---------|
| `0` | All checks returned **OK** or **INFO** — cluster is healthy |
| `1` | At least one check returned **WARN** — investigate before next incident window |
| `2` | At least one check returned **CRITICAL** — requires immediate attention |

```bash
# Use in CI — fail the pipeline on any CRITICAL finding
./pg-healthcheck --host $DB_HOST --user postgres --dbname $DB_NAME
# $? is 0, 1, or 2

# Use in shell scripts
if ! ./pg-healthcheck --host $DB_HOST --user postgres; then
  echo "Health check failed (exit $?)" | send_alert
fi
```

JSON output (`--output json`) preserves the same exit code semantics,
making it easy to pipe results into Prometheus, Datadog, or any
monitoring system that accepts structured data.

## Prerequisites

The following software must be present before building or running
pg-healthcheck:

- Go 1.26.10 or later. Install with `brew install go` on macOS or follow
  the [Go installation guide](https://go.dev/doc/install).
- PostgreSQL 13 or later as the target database. Checks that require
  PostgreSQL 14, 15, 16, or 17 features skip gracefully on older
  versions.
- The `pg_monitor` built-in role on the target database. This role
  grants read access to all `pg_stat_*` views and catalog functions
  without requiring superuser. Some G11 security checks and G07
  `amcheck` verification return partial results without superuser.
- pgBackRest (optional). G02 backup checks skip gracefully with a
  clear message if pgBackRest is not installed.
- The `amcheck` extension (optional). G07 B-tree and heap integrity
  checks skip if the extension is absent. Install it with
  `CREATE EXTENSION amcheck;`.
- The pgEdge Spock extension (optional). G12 emits a single INFO
  finding and skips all checks if Spock is not installed; the tool
  is safe to run on standard PostgreSQL.

## Building from Source

Clone the repository and build the binary using the following
commands:

```bash
git clone https://github.com/ahsanhadi/pg-healthcheck.git
cd pg-healthcheck
go build -o pg-healthcheck ./cmd/pg-healthcheck
```

The resulting `pg-healthcheck` binary is self-contained and has no
runtime dependencies.

To verify the build, run the following command against a local
PostgreSQL instance:

```bash
./pg-healthcheck --host localhost --dbname postgres --user postgres
```

## Cross-Platform Builds

The tool builds for Linux (amd64, arm64), macOS (amd64, arm64), and
Windows (amd64). The CI pipeline verifies all platforms on every push.
To cross-compile manually, set the `GOOS` and `GOARCH` environment
variables before building:

```bash
GOOS=linux GOARCH=amd64 go build -o pg-healthcheck-linux-amd64 ./cmd/pg-healthcheck
```

## Running in CI

pg-healthcheck integrates naturally into CI pipelines using its exit
codes. A non-zero exit code signals health issues:

```bash
./pg-healthcheck --host $DB_HOST --user postgres && echo "healthy"
```

Exit code `1` indicates at least one WARN finding. Exit code `2`
indicates at least one CRITICAL finding. Exit code `0` means all
checks returned OK or INFO.

## Next Steps

- The [Configuration Reference](configuration.md) document explains
  every threshold in `healthcheck.yaml` and how to override defaults
  for your workload.
- The [CLI Usage and Modes](usage.md) document covers all command-line
  flags, operating modes, and practical examples.
