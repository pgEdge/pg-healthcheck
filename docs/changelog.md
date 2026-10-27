# Changelog

All notable changes to pg-healthcheck will be documented in this file.

The format is based on
[Keep a Changelog](https://keepachangelog.com/), and this project
adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- G15 group - Replication Health - with three new checks: WAL
  receiver status (G15-001), inactive physical replication slots
  (G15-002), and standby query conflicts broken down by type
  (G15-003).
- G04-011 - Slow query count from `pg_stat_statements`, counting
  normalized query patterns whose mean execution time exceeds a
  configurable threshold (default 5000ms).
- G11-011 - Unencrypted client connections, detecting non-loopback
  client backends connected without SSL/TLS via `pg_stat_ssl`.
- `slow_query_mean_warn_ms` configuration key for G04-011 threshold
  tuning.

### Changed

- G13-007 (max_connections advisory) now adds a WARN tier at
  `max_connections > 500` in addition to the existing INFO at > 200.
- G14-010 (WAL archiver status) now uses `last_failed_time` recency
  instead of cumulative `failed_count`, so old resolved failures no
  longer trigger perpetual alerts. A failure within the last hour
  triggers CRITICAL; a failure within 24 hours triggers WARN.
- Go raised to 1.26.10 so that builds distrust the compromised eMudhra
  intermediate CAs (CVE-2026-111943).

## [0.2.0] - 2026-04-22

### Added

- G14-015 - WAL accumulation since last checkpoint, alerting when
  long-running transactions block WAL segment recycling.
- G12-023 - Replication LSN lag in MB between each Spock node pair
  from `spock.progress`.
- G12-022 - Per-subscription conflict and DCA counters from
  `spock.channel_summary_stats`.
- G13-008 through G13-011 - OS-level checks for Transparent Huge
  Pages, CPU frequency governor, data directory disk space, and
  postmaster uptime.
- G09-009 through G09-013 - Logical replication slot health checks
  covering invalidated slots, missing workers, and subscription
  relation sync state.

### Fixed

- G12-023 now scans timestamp columns into `time.Time` instead of
  `string`, eliminating scan errors on Spock progress views.

## [0.1.0] - Initial Release

### Added

- Initial release with G01 through G14 check groups covering
  connection health, pgBackRest backup, performance settings, lock
  contention, vacuum and bloat, index health, TOAST and data
  integrity, visibility map, WAL and replication slots, upgrade
  readiness, security posture, pgEdge Spock cluster, OS resources,
  and WAL growth.
- Single-node and cluster operating modes.
- Text and JSON output formats.
- Configurable thresholds via `healthcheck.yaml`.
- Cross-platform builds for Linux, macOS, and Windows.
