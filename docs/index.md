<div class="banner" markdown>
![pgEdge Labs](img/pgedge-labs-light.svg#only-light){ width="320" }
![pgEdge Labs](img/pgedge-labs-dark.svg#only-dark){ width="320" }
</div>

# pg-healthcheck

pg-healthcheck is an enterprise-grade PostgreSQL health diagnostics
tool for single instances and pgEdge multi-node Spock clusters. The
tool runs 180+ checks across 15 groups by querying live PostgreSQL
system catalog views - no estimates and no simulated data. Output is
coloured terminal text or structured JSON for GUI and API consumption.

## Overview

pg-healthcheck includes the following capabilities:

- comprehensive diagnostics across connections, backups, performance,
  locks, vacuum, indexes, WAL, replication, security, and OS resources.
- single-node and cluster (pgEdge Spock) modes with cross-node
  row-count parity checks.
- configurable warn and critical thresholds via a YAML configuration
  file.
- JSON output mode for integration with dashboards and monitoring
  pipelines.
- graceful skip behavior when optional extensions or features are
  absent.
- checks verified against live PostgreSQL system catalog views on
  PostgreSQL 13 through 17.

## Quick Start

The following commands show how to connect to a local PostgreSQL
instance and run all checks:

```bash
./pg-healthcheck --host localhost --dbname mydb --user postgres
```

To run a subset of check groups, use the `--groups` flag:

```bash
./pg-healthcheck --groups G01,G05,G09 --verbose
```

To output results as JSON:

```bash
./pg-healthcheck --output json | jq '.summary'
```

## Next Steps

- The [Architecture](architecture.md) document describes the internal
  layers of pg-healthcheck and how checks are organized.
- The [Installation](getting_started.md) document explains how to
  build and install the tool from source.
- The [Configuration Reference](configuration.md) document covers
  every tunable threshold in `healthcheck.yaml`.
- The [Check Groups Reference](check_reference.md) document lists all
  15 check groups and their individual checks.
