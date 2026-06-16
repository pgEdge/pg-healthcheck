//go:build integration

// Package tests contains integration tests for pg-healthcheck.
//
// These tests inject real problems into a PostgreSQL database, run the full
// check suite, and assert that the expected check IDs fire at the expected
// severity levels.  They require a live PostgreSQL instance and are NOT run
// during normal `go test ./...` — they must be opted-in explicitly:
//
//	go test -tags integration -v ./tests/ \
//	    -pg-host 192.168.169.158 -pg-port 5432 \
//	    -pg-dbname testdb -pg-user ahsan
//
// The test creates all objects in the _hc_test schema and drops it on cleanup,
// so it is safe to run against a non-production database.
package tests

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ── flags ──────────────────────────────────────────────────────────────────

var (
	pgHost   = flag.String("pg-host", "localhost", "PostgreSQL host")
	pgPort   = flag.Int("pg-port", 5432, "PostgreSQL port")
	pgDBName = flag.String("pg-dbname", "postgres", "Database name")
	pgUser   = flag.String("pg-user", "postgres", "PostgreSQL user")
	pgPass   = flag.String("pg-pass", os.Getenv("PGPASSWORD"), "PostgreSQL password")
	binary   = flag.String("binary", "./pg-healthcheck", "Path to pg-healthcheck binary")
)

// ── helpers ────────────────────────────────────────────────────────────────

type checkResult struct {
	CheckID  string `json:"check_id"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Observed string `json:"observed"`
	Detail   string `json:"detail"`
	NodeName string `json:"node_name"`
}

type report struct {
	Checks []checkResult `json:"checks"`
}

// runBinary resolves the binary, executes it with args from the repo root, parses
// JSON output, and returns a check-ID-keyed results map. Shared by runHealthcheck
// and runAsk to avoid duplicating binary resolution and JSON parsing.
func runBinary(t *testing.T, args []string) map[string]checkResult {
	t.Helper()
	// Go test changes CWD to the package directory (tests/) before running,
	// so binary paths like "./pg-healthcheck" are relative to tests/ — not the
	// repo root where the binary is actually built.  Resolve from ".." (repo root)
	// before absolutising so we find the real binary.
	binPath, err := filepath.Abs(filepath.Join("..", *binary))
	if err != nil {
		t.Fatalf("failed to resolve binary path %q: %v", *binary, err)
	}
	cmd := exec.Command(binPath, args...) //nolint:gosec // nosemgrep -- test binary path resolved via filepath.Abs from repo root; args are controlled test flags
	// Run from repo root so default ./healthcheck.yaml is picked up.
	// Running from tests/ leaves cfg.CheckTimeout at zero (no YAML load),
	// causing near-immediate context deadline exceeded in many checks.
	cmd.Dir = ".."
	out, _ := cmd.Output() // non-zero exit is expected when there are WARNs/CRITs

	var r report
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("failed to parse binary output: %v\nraw: %s", err, string(out))
	}
	results := make(map[string]checkResult, len(r.Checks))
	for _, c := range r.Checks {
		results[c.CheckID] = c
	}
	return results
}

// runHealthcheck executes the binary and returns parsed findings.
func runHealthcheck(t *testing.T, extraArgs ...string) map[string]checkResult {
	t.Helper()
	args := []string{
		"--host", *pgHost,
		"--port", fmt.Sprintf("%d", *pgPort),
		"--dbname", *pgDBName,
		"--user", *pgUser,
		"--output", "json",
		"--verbose",
	}
	if *pgPass != "" {
		args = append(args, "--password", *pgPass)
	}
	args = append(args, extraArgs...)
	return runBinary(t, args)
}

// db opens a direct connection for injecting/cleaning test conditions.
func db(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := fmt.Sprintf("host=%s port=%d dbname=%s user=%s",
		*pgHost, *pgPort, *pgDBName, *pgUser)
	if *pgPass != "" {
		dsn += " password=" + *pgPass
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("cannot connect to PostgreSQL: %v", err)
	}
	return pool
}

// exec runs a SQL statement and fails the test on error.
func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil { //nolint:gosec // nosemgrep -- callers always pass hardcoded SQL literals; args are parameterised query placeholders
		t.Fatalf("SQL failed: %v\nSQL: %s", err, sql)
	}
}

// assertSeverity checks that a given check ID produced the expected severity.
func assertSeverity(t *testing.T, results map[string]checkResult, checkID, wantSev string) {
	t.Helper()
	c, ok := results[checkID]
	if !ok {
		t.Errorf("check %s not found in output", checkID)
		return
	}
	if c.Severity != wantSev {
		t.Errorf("check %s: got severity %q, want %q (observed: %s)",
			checkID, c.Severity, wantSev, c.Observed)
	}
}

// ── setup / teardown ───────────────────────────────────────────────────────

func setupSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, pool, `CREATE SCHEMA IF NOT EXISTS _hc_test`)
}

func teardown(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// Roll back any dangling prepared transaction
	pool.Exec(ctx, `ROLLBACK PREPARED '_hc_test_prepared_tx'`) //nolint:gosec // nosemgrep
	// Drop inactive replication slot
	pool.Exec(ctx, //nolint:gosec // nosemgrep
		`SELECT pg_drop_replication_slot('_hc_test_inactive_slot')
		                WHERE EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name='_hc_test_inactive_slot')`)
	// Drop test superuser
	pool.Exec(ctx, `DROP ROLE IF EXISTS _hc_test_super`) //nolint:gosec // nosemgrep
	// Drop lock-diagnostic test roles
	pool.Exec(ctx, `DROP ROLE IF EXISTS _hc_test_blocker`) //nolint:gosec // nosemgrep
	pool.Exec(ctx, `DROP ROLE IF EXISTS _hc_test_waiter`)  //nolint:gosec // nosemgrep
	// Revoke public schema create
	pool.Exec(ctx, `REVOKE CREATE ON SCHEMA public FROM PUBLIC`) //nolint:gosec // nosemgrep
	// Drop all test objects
	mustExec(t, pool, `DROP SCHEMA IF EXISTS _hc_test CASCADE`)
}

// ── tests ──────────────────────────────────────────────────────────────────

// TestG05_DeadTuples verifies that a table with a high dead-tuple ratio
// triggers the autovacuum / dead tuple checks.
func TestG05_DeadTuples(t *testing.T) {
	pool := db(t)
	defer pool.Close()
	setupSchema(t, pool)
	defer teardown(t, pool)

	mustExec(t, pool, `CREATE TABLE _hc_test.bloat(id serial PRIMARY KEY, data text)`)
	// Insert 60k rows so that after 80% deletion (id%5!=0), 12k live rows remain.
	// G05-003 requires n_live_tup > 10000 AND n_dead_tup > n_live_tup*0.2.
	mustExec(t, pool, `INSERT INTO _hc_test.bloat(data) SELECT repeat('x',200) FROM generate_series(1,60000)`)
	mustExec(t, pool, `DELETE FROM _hc_test.bloat WHERE id % 5 != 0`)
	// Flush stats so pg_stat_user_tables reflects the new dead-tuple counts immediately.
	// pg_stat_force_next_flush() was added in PG 15; on older versions we clear the
	// cached snapshot and sleep briefly so the stats collector can catch up.
	mustExec(t, pool, `DO $$
BEGIN
  IF current_setting('server_version_num')::int >= 150000 THEN
    PERFORM pg_stat_force_next_flush();
  ELSE
    PERFORM pg_sleep(0.5);
    PERFORM pg_stat_clear_snapshot();
  END IF;
END$$`)
	// do NOT vacuum — leave dead tuples in place

	results := runHealthcheck(t, "--groups", "G05")
	// G05-003 checks dead tuple ratio
	c, ok := results["G05-003"]
	if !ok {
		t.Error("G05-003 not found in output")
		return
	}
	if c.Severity != "WARN" && c.Severity != "CRITICAL" {
		t.Errorf("G05-003: expected WARN or CRITICAL, got %s — may need more dead tuples", c.Severity)
	}
	t.Logf("G05-003: %s — %s", c.Severity, c.Observed)
}

// TestG05_PreparedTransactions verifies that an old prepared transaction
// is detected by G05-013.
func TestG05_PreparedTransactions(t *testing.T) {
	pool := db(t)
	defer pool.Close()
	setupSchema(t, pool)
	defer teardown(t, pool)

	// Prepared transactions cannot be issued through pgx in the same pool —
	// use a single-connection pool to avoid the autocommit wrapper.
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	ctx := context.Background()
	conn.Exec(ctx, `ROLLBACK PREPARED '_hc_test_prepared_tx'`)                              //nolint:gosec // nosemgrep -- clean prior run, static SQL
	conn.Exec(ctx, `BEGIN`)                                                                 //nolint:gosec // nosemgrep
	conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS _hc_test.pt_marker(id int)`)                 //nolint:gosec // nosemgrep
	if _, err := conn.Exec(ctx, `PREPARE TRANSACTION '_hc_test_prepared_tx'`); err != nil { //nolint:gosec // nosemgrep
		t.Skipf("PREPARE TRANSACTION not allowed (max_prepared_transactions=0?): %v", err)
	}

	results := runHealthcheck(t, "--groups", "G05")
	c, ok := results["G05-013"]
	if !ok {
		t.Error("G05-013 not found in output")
		return
	}
	if c.Severity == "OK" {
		t.Errorf("G05-013: expected INFO or WARN, got OK — prepared transaction not detected")
	}
	t.Logf("G05-013: %s — %s", c.Severity, c.Observed)
}

// TestG06_DuplicateIndexes verifies G06-002 fires when two indexes cover
// the same column on the same table.
func TestG06_DuplicateIndexes(t *testing.T) {
	pool := db(t)
	defer pool.Close()
	setupSchema(t, pool)
	defer teardown(t, pool)

	mustExec(t, pool, `CREATE TABLE _hc_test.dupidx(id serial PRIMARY KEY, name text)`)
	mustExec(t, pool, `INSERT INTO _hc_test.dupidx(name) SELECT 'u'||g FROM generate_series(1,500) g`)
	mustExec(t, pool, `CREATE INDEX _hc_test_dupidx_a ON _hc_test.dupidx(name)`)
	mustExec(t, pool, `CREATE INDEX _hc_test_dupidx_b ON _hc_test.dupidx(name)`)

	results := runHealthcheck(t, "--groups", "G06")
	assertSeverity(t, results, "G06-002", "WARN")
	t.Logf("G06-002: %s — %s", results["G06-002"].Severity, results["G06-002"].Observed)
}

// TestG06_InvalidIndex verifies G06-003 fires when an index has indisvalid=false.
func TestG06_InvalidIndex(t *testing.T) {
	pool := db(t)
	defer pool.Close()
	setupSchema(t, pool)
	defer teardown(t, pool)

	mustExec(t, pool, `CREATE TABLE _hc_test.invalidtbl(id serial PRIMARY KEY, val int)`)
	mustExec(t, pool, `INSERT INTO _hc_test.invalidtbl(val) SELECT g FROM generate_series(1,100) g`)
	mustExec(t, pool, `CREATE INDEX _hc_test_invalid_idx ON _hc_test.invalidtbl(val)`)
	// Simulate a broken concurrent reindex by flipping indisvalid
	mustExec(t, pool, `UPDATE pg_index SET indisvalid=false
	                   WHERE indexrelid='_hc_test._hc_test_invalid_idx'::regclass`)

	results := runHealthcheck(t, "--groups", "G06")
	assertSeverity(t, results, "G06-003", "CRITICAL")
	t.Logf("G06-003: %s — %s", results["G06-003"].Severity, results["G06-003"].Observed)
}

// TestG06_TableWithoutPK verifies G06-010 fires when a user table has no PK.
func TestG06_TableWithoutPK(t *testing.T) {
	pool := db(t)
	defer pool.Close()
	setupSchema(t, pool)
	defer teardown(t, pool)

	mustExec(t, pool, `CREATE TABLE _hc_test.nopk(id int, val text)`)
	mustExec(t, pool, `INSERT INTO _hc_test.nopk VALUES(1,'no pk')`)

	results := runHealthcheck(t, "--groups", "G06")
	assertSeverity(t, results, "G06-010", "WARN")
	t.Logf("G06-010: %s — %s", results["G06-010"].Severity, results["G06-010"].Observed)
}

// TestG09_InactiveSlot verifies G09-001 fires when a replication slot is
// inactive and retaining WAL.
func TestG09_InactiveSlot(t *testing.T) {
	pool := db(t)
	defer pool.Close()
	setupSchema(t, pool)
	defer teardown(t, pool)

	pool.Exec(context.Background(), //nolint:gosec // nosemgrep
		`SELECT pg_drop_replication_slot('_hc_test_inactive_slot')
		                WHERE EXISTS(SELECT 1 FROM pg_replication_slots
		                             WHERE slot_name='_hc_test_inactive_slot')`)
	mustExec(t, pool, `SELECT pg_create_physical_replication_slot('_hc_test_inactive_slot', false, false)`)

	// Generate some WAL so the slot falls behind
	mustExec(t, pool, `CREATE TABLE IF NOT EXISTS _hc_test.wal_gen(id serial PRIMARY KEY, d text)`)
	for i := 0; i < 10; i++ {
		mustExec(t, pool, `INSERT INTO _hc_test.wal_gen(d) SELECT repeat('x',1000) FROM generate_series(1,100)`)
	}

	results := runHealthcheck(t, "--groups", "G09")
	c, ok := results["G09-001"]
	if !ok {
		t.Error("G09-001 not found in output")
		return
	}
	// Slot is inactive — must not be OK regardless of how much WAL has accumulated.
	if c.Severity == "OK" {
		t.Errorf("G09-001: expected INFO/WARN/CRIT for inactive slot, got OK — %s", c.Observed)
	} else {
		t.Logf("G09-001: %s — %s ✓", c.Severity, c.Observed)
	}
}

// TestG11_PublicSchemaCreate verifies G11-003 fires when PUBLIC has CREATE
// on the public schema.
func TestG11_PublicSchemaCreate(t *testing.T) {
	pool := db(t)
	defer pool.Close()
	setupSchema(t, pool)
	defer teardown(t, pool)

	mustExec(t, pool, `GRANT CREATE ON SCHEMA public TO PUBLIC`)

	results := runHealthcheck(t, "--groups", "G11")
	assertSeverity(t, results, "G11-003", "WARN")
	t.Logf("G11-003: %s — %s", results["G11-003"].Severity, results["G11-003"].Observed)
}

// TestG11_ExtraSuperuser verifies G11-009 fires when there are > 2 superuser
// login roles.
func TestG11_ExtraSuperuser(t *testing.T) {
	pool := db(t)
	defer pool.Close()
	setupSchema(t, pool)
	defer teardown(t, pool)

	ctx := context.Background()
	pool.Exec(ctx, `DROP ROLE IF EXISTS _hc_test_super`) //nolint:gosec // nosemgrep
	mustExec(t, pool, `CREATE ROLE _hc_test_super SUPERUSER LOGIN PASSWORD 'testonly123!'`)

	results := runHealthcheck(t, "--groups", "G11")
	c, ok := results["G11-009"]
	if !ok {
		t.Error("G11-009 not found")
		return
	}
	if c.Severity != "WARN" && c.Severity != "INFO" {
		t.Errorf("G11-009: expected WARN or INFO, got %s", c.Severity)
	}
	t.Logf("G11-009: %s — %s", c.Severity, c.Observed)
}

// TestG01_HBATrust verifies G01-009 fires when pg_hba.conf has a TRUST
// rule on a non-loopback address. (This is a read-only check — no injection
// needed if the test database already has it configured.)
func TestG01_HBATrust(t *testing.T) {
	results := runHealthcheck(t, "--groups", "G01")
	c, ok := results["G01-009"]
	if !ok {
		t.Skip("G01-009 not in output — pg_hba_file_rules may not be accessible")
	}
	t.Logf("G01-009: %s — %s", c.Severity, c.Observed)
	// If TRUST is present, it should be CRITICAL
	if c.Severity != "OK" && c.Severity != "CRITICAL" {
		t.Errorf("G01-009: unexpected severity %s", c.Severity)
	}
}

// TestAllChecksPresent is a smoke test that verifies all expected check IDs
// appear in the output — catches regressions where a check is accidentally
// removed or no longer wired into Run().
func TestAllChecksPresent(t *testing.T) {
	results := runHealthcheck(t)

	required := []string{
		// G01
		"G01-001", "G01-002", "G01-003", "G01-005", "G01-006", "G01-007",
		// G03
		"G03-001", "G03-002", "G03-007", "G03-008",
		// G04
		"G04-001", "G04-002", "G04-003", "G04-004", "G04-011",
		// G05
		"G05-001", "G05-002", "G05-003", "G05-012", "G05-013",
		// G06
		"G06-001", "G06-002", "G06-003", "G06-004", "G06-010",
		// G09
		"G09-001", "G09-004", "G09-009", "G09-014",
		// G11
		"G11-001", "G11-002", "G11-003", "G11-009", "G11-010", "G11-011",
		// G14
		"G14-001", "G14-002", "G14-005", "G14-013",
		// G15
		"G15-001", "G15-002", "G15-003",
	}

	missing := 0
	for _, id := range required {
		if _, ok := results[id]; !ok {
			t.Errorf("required check %s missing from output", id)
			missing++
		}
	}
	t.Logf("Total checks in output: %d", len(results))
	t.Logf("Missing required checks: %d", missing)

	// Report timing
	start := time.Now()
	_ = runHealthcheck(t)
	t.Logf("Full run time: %v", time.Since(start))
}

// TestG04_LockRoleAppDiagnostics verifies G04-003 groups lock contention by
// role + application_name and emits redacted summary details.
func TestG04_LockRoleAppDiagnostics(t *testing.T) {
	pool := db(t)
	defer pool.Close()
	setupSchema(t, pool)
	defer teardown(t, pool)

	mustExec(t, pool, `CREATE TABLE _hc_test.lock_diag(id int PRIMARY KEY)`)
	mustExec(t, pool, `INSERT INTO _hc_test.lock_diag VALUES (1)`)

	mustExec(t, pool, `DROP ROLE IF EXISTS _hc_test_blocker`)
	mustExec(t, pool, `DROP ROLE IF EXISTS _hc_test_waiter`)
	mustExec(t, pool, `CREATE ROLE _hc_test_blocker LOGIN PASSWORD 'hc_blocker_pw'`)
	mustExec(t, pool, `CREATE ROLE _hc_test_waiter LOGIN PASSWORD 'hc_waiter_pw'`)
	mustExec(t, pool, `GRANT USAGE ON SCHEMA _hc_test TO _hc_test_blocker, _hc_test_waiter`)
	// ACCESS EXCLUSIVE lock test needs stronger relation privileges than SELECT.
	mustExec(t, pool, `GRANT ALL PRIVILEGES ON _hc_test.lock_diag TO _hc_test_blocker, _hc_test_waiter`)

	blockerDSN := fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s application_name=%s pool_max_conns=1",
		*pgHost, *pgPort, *pgDBName, "_hc_test_blocker", "hc_blocker_pw", "hc_blocker_app",
	)
	// Intentionally do NOT set application_name here; should normalize to "(unset)".
	waiterDSN := fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s pool_max_conns=1",
		*pgHost, *pgPort, *pgDBName, "_hc_test_waiter", "hc_waiter_pw",
	)

	blockerPool, err := pgxpool.New(context.Background(), blockerDSN)
	if err != nil {
		t.Fatalf("blocker connect: %v", err)
	}
	defer blockerPool.Close()
	waiterPool, err := pgxpool.New(context.Background(), waiterDSN)
	if err != nil {
		t.Fatalf("waiter connect: %v", err)
	}
	defer waiterPool.Close()

	ctx := context.Background()
	blockerConn, err := blockerPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire blocker conn: %v", err)
	}
	defer blockerConn.Release()
	if _, err := blockerConn.Exec(ctx, `BEGIN`); err != nil { //nolint:gosec // nosemgrep
		t.Fatalf("blocker BEGIN failed: %v", err)
	}
	if _, err := blockerConn.Exec(ctx, `LOCK TABLE _hc_test.lock_diag IN ACCESS EXCLUSIVE MODE`); err != nil { //nolint:gosec // nosemgrep
		t.Fatalf("blocker LOCK failed: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	waiterDone := make(chan error, 1)
	go func() {
		defer wg.Done()
		waiterConn, err := waiterPool.Acquire(ctx)
		if err != nil {
			waiterDone <- err
			return
		}
		defer waiterConn.Release()
		_, err = waiterConn.Exec(ctx, `BEGIN`) //nolint:gosec // nosemgrep
		if err != nil {
			waiterDone <- err
			return
		}
		_, err = waiterConn.Exec(ctx, `LOCK TABLE _hc_test.lock_diag IN ACCESS SHARE MODE`) //nolint:gosec // nosemgrep
		if err == nil {
			_, _ = waiterConn.Exec(ctx, `ROLLBACK`) //nolint:gosec // nosemgrep
		}
		waiterDone <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(context.Background(), //nolint:gosec // nosemgrep
			`SELECT count(*) FROM pg_stat_activity
			WHERE usename = '_hc_test_waiter'
			  AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("lock wait probe failed: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter did not enter lock wait state in time")
		}
		time.Sleep(100 * time.Millisecond)
	}

	var c checkResult
	var got bool
	for attempt := 1; attempt <= 5; attempt++ {
		// Ensure the waiter is still blocked right before invoking healthcheck.
		var blockedNow int
		if err := pool.QueryRow(context.Background(), //nolint:gosec // nosemgrep
			`SELECT count(*)
			FROM pg_stat_activity
			WHERE usename = '_hc_test_waiter'
			  AND wait_event_type = 'Lock'
			  AND cardinality(pg_blocking_pids(pid)) > 0`).Scan(&blockedNow); err != nil {
			t.Fatalf("lock wait probe before healthcheck failed: %v", err)
		}
		if blockedNow == 0 {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		results := runHealthcheck(t, "--groups", "G04")
		var ok bool
		c, ok = results["G04-003"]
		if !ok {
			t.Fatalf("G04-003 not found in output on attempt %d", attempt)
		}
		if c.Severity == "WARN" {
			got = true
			break
		}
		// Small retry window to avoid race with lock state transition timing.
		time.Sleep(200 * time.Millisecond)
	}
	if !got {
		t.Fatalf("G04-003 severity: got %s want WARN (observed: %s)", c.Severity, c.Observed)
	}
	if !strings.Contains(c.Detail, "Top waiting groups (role/app):") {
		t.Fatalf("G04-003 detail missing waiting summary: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "Top blocker groups (role/app):") {
		t.Fatalf("G04-003 detail missing blocker summary: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "waiter role=_hc_test_waiter") {
		t.Fatalf("G04-003 detail missing waiter role mapping: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "blocker role=_hc_test_blocker") {
		t.Fatalf("G04-003 detail missing blocker role mapping: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "app=(unset)") {
		t.Fatalf("G04-003 detail missing app normalization '(unset)': %q", c.Detail)
	}
	if strings.Contains(strings.ToLower(c.Detail), "lock table") || strings.Contains(strings.ToLower(c.Detail), "select ") {
		t.Fatalf("G04-003 detail appears to contain SQL text, expected redacted summary only: %q", c.Detail)
	}

	// Unblock and clean up waiter transaction.
	if _, err := blockerConn.Exec(ctx, `ROLLBACK`); err != nil { //nolint:gosec // nosemgrep
		t.Fatalf("blocker ROLLBACK failed: %v", err)
	}
	if err := <-waiterDone; err != nil {
		t.Fatalf("waiter lock statement returned error: %v", err)
	}
	wg.Wait()
}

// TestG04_NoLockBlockingPath verifies the existing no-contention behavior
// remains intact and does not emit summary detail noise.
func TestG04_NoLockBlockingPath(t *testing.T) {
	results := runHealthcheck(t, "--groups", "G04")
	c, ok := results["G04-003"]
	if !ok {
		t.Fatal("G04-003 not found in output")
	}
	// Shared test environments may have ambient lock contention.
	if c.Severity != "OK" {
		t.Skipf("environment has active lock contention; skipping no-contention assertion (%s: %s)", c.Severity, c.Observed)
	}
	if c.Observed != "No lock blocking detected" {
		t.Fatalf("unexpected G04-003 observed in OK path: %q", c.Observed)
	}
	if strings.TrimSpace(c.Detail) != "" {
		t.Fatalf("expected empty detail in no-contention path, got: %q", c.Detail)
	}
}

// TestG07_TOASTCorruption verifies that G07-003 fires CRITICAL when a table's
// reltoastrelid points to a non-existent pg_class entry (broken TOAST reference),
// and that G07-005 fires WARN when a TOAST table has no parent (orphaned).
func TestG07_TOASTCorruption(t *testing.T) {
	pool := db(t)
	defer pool.Close()
	setupSchema(t, pool)
	defer teardown(t, pool)

	// Create a table large enough to get a real TOAST table allocated.
	mustExec(t, pool, `CREATE TABLE _hc_test.toast_corrupt(id serial PRIMARY KEY, data text)`)
	mustExec(t, pool, `INSERT INTO _hc_test.toast_corrupt(data) SELECT repeat('x', 10000) FROM generate_series(1, 5)`)

	// Verify the table actually got a TOAST table before we corrupt it.
	var toastOID uint32
	err := pool.QueryRow(context.Background(), //nolint:gosec // nosemgrep
		`SELECT reltoastrelid FROM pg_class WHERE oid = '_hc_test.toast_corrupt'::regclass`).Scan(&toastOID)
	if err != nil || toastOID == 0 {
		t.Skipf("table has no TOAST table (toastOID=%d, err=%v) — skipping", toastOID, err)
	}
	t.Logf("original reltoastrelid = %d", toastOID)

	// Break the reltoastrelid pointer — point it to a non-existent OID.
	// This creates the dangling reference that G07-003 detects.
	mustExec(t, pool,
		`UPDATE pg_class SET reltoastrelid = 2147483647 WHERE oid = '_hc_test.toast_corrupt'::regclass`)

	results := runHealthcheck(t, "--groups", "G07")

	// G07-003: broken reltoastrelid → CRITICAL
	c003, ok := results["G07-003"]
	if !ok {
		t.Error("G07-003 not found in output")
	} else if c003.Severity != "CRITICAL" {
		t.Errorf("G07-003: expected CRITICAL for broken TOAST ref, got %s — %s", c003.Severity, c003.Observed)
	} else {
		t.Logf("G07-003: %s — %s ✓", c003.Severity, c003.Observed)
	}

	// G07-005: the original TOAST table now has no parent → WARN
	c005, ok := results["G07-005"]
	if !ok {
		t.Error("G07-005 not found in output")
	} else if c005.Severity != "WARN" {
		t.Errorf("G07-005: expected WARN for orphaned TOAST table, got %s — %s", c005.Severity, c005.Observed)
	} else {
		t.Logf("G07-005: %s — %s ✓", c005.Severity, c005.Observed)
	}

	// Restore the pointer so teardown can DROP SCHEMA cleanly.
	mustExec(t, pool,
		`UPDATE pg_class SET reltoastrelid = $1 WHERE oid = '_hc_test.toast_corrupt'::regclass`, toastOID)
}

// runAsk executes the binary's `ask` subcommand and returns parsed findings.
// It uses --ollama-timeout 1 so the test never actually waits for Ollama —
// the timeout forces keyword-fallback mode immediately.
func runAsk(t *testing.T, query string, extraArgs ...string) map[string]checkResult {
	t.Helper()
	args := []string{
		"ask", query,
		"--host", *pgHost,
		"--port", fmt.Sprintf("%d", *pgPort),
		"--dbname", *pgDBName,
		"--user", *pgUser,
		"--output", "json",
		"--verbose",
		"--ollama-host", "http://127.0.0.1:19999", // guaranteed unreachable → keyword fallback
		"--ollama-timeout", "1",
	}
	if *pgPass != "" {
		args = append(args, "--password", *pgPass)
	}
	args = append(args, extraArgs...)
	return runBinary(t, args)
}

// TestAsk_ToastKeyword verifies that asking about TOAST corruption runs G07 checks.
func TestAsk_ToastKeyword(t *testing.T) {
	results := runAsk(t, "check for TOAST table corruption")
	// G07-001 (data checksums) should always appear when G07 runs.
	if _, ok := results["G07-001"]; !ok {
		t.Error("G07-001 not found — G07 group was not run by the ask subcommand")
	}
	t.Logf("ask 'TOAST corruption' correctly ran G07 (%d findings)", len(results))
}

// TestAsk_WALDisk verifies that asking about WAL disk size runs G14 (and/or G09).
func TestAsk_WALDisk(t *testing.T) {
	results := runAsk(t, "check WAL disk size")
	// G14-001 is the WAL directory size check — always present when G14 runs.
	if _, ok := results["G14-001"]; !ok {
		// G09 slot checks are also WAL-related; accept either group.
		if _, ok2 := results["G09-001"]; !ok2 {
			t.Error("neither G14-001 nor G09-001 found — ask did not map 'WAL disk size' to a WAL group")
		}
	}
	t.Logf("ask 'WAL disk size' correctly ran WAL group(s) (%d findings)", len(results))
}

// TestAsk_LockContention verifies that asking about locks runs G04.
func TestAsk_LockContention(t *testing.T) {
	results := runAsk(t, "are there any lock contention issues or deadlocks?")
	if _, ok := results["G04-003"]; !ok {
		t.Error("G04-003 not found — G04 was not run by the ask subcommand for a locks query")
	}
	t.Logf("ask 'lock contention' correctly ran G04 (%d findings)", len(results))
}

// TestAsk_Security verifies that asking about security runs G11.
func TestAsk_Security(t *testing.T) {
	results := runAsk(t, "check superuser count and schema permissions")
	found := false
	for id := range results {
		if strings.HasPrefix(id, "G11-") {
			found = true
			break
		}
	}
	if !found {
		t.Error("no G11 findings — G11 was not run by the ask subcommand for a security query")
	}
	t.Logf("ask 'security' correctly ran G11 (%d findings)", len(results))
}

// TestAsk_Replication verifies that asking about replication lag runs G15 or G09.
func TestAsk_Replication(t *testing.T) {
	results := runAsk(t, "check replication lag on standby")
	foundRepl := false
	for id := range results {
		if strings.HasPrefix(id, "G15-") || strings.HasPrefix(id, "G09-") {
			foundRepl = true
			break
		}
	}
	if !foundRepl {
		t.Error("no G15/G09 findings — ask did not map 'replication lag' to a replication group")
	}
	t.Logf("ask 'replication lag' correctly ran replication group(s) (%d findings)", len(results))
}

// TestAsk_VacuumBloat verifies that asking about dead tuples runs G05.
func TestAsk_VacuumBloat(t *testing.T) {
	results := runAsk(t, "check for dead tuples and table bloat")
	found := false
	for id := range results {
		if strings.HasPrefix(id, "G05-") {
			found = true
			break
		}
	}
	if !found {
		t.Error("no G05 findings — G05 was not run by the ask subcommand for a vacuum/bloat query")
	}
	t.Logf("ask 'dead tuples' correctly ran G05 (%d findings)", len(results))
}

// TestAsk_Connection verifies that asking about connection/SSL runs G01.
func TestAsk_Connection(t *testing.T) {
	results := runAsk(t, "check SSL certificate expiry and connection saturation")
	found := false
	for id := range results {
		if strings.HasPrefix(id, "G01-") {
			found = true
			break
		}
	}
	if !found {
		t.Error("no G01 findings — G01 was not run by the ask subcommand for a connection query")
	}
	t.Logf("ask 'SSL/connection' correctly ran G01 (%d findings)", len(results))
}

// TestAsk_JSONOutput verifies that the ask subcommand can output valid JSON.
func TestAsk_JSONOutput(t *testing.T) {
	// runAsk already uses --output json; just ensure we get valid structured output.
	results := runAsk(t, "check for index issues")
	if len(results) == 0 {
		t.Error("ask returned no findings in JSON mode")
	}
	t.Logf("ask JSON output: %d findings", len(results))
}

// TestG08_VMHeapMismatch verifies that G08-002 fires WARN when pg_class.relallvisible
// exceeds relpages — the catalog-level signal for a stale or corrupt visibility map.
func TestG08_VMHeapMismatch(t *testing.T) {
	pool := db(t)
	defer pool.Close()
	setupSchema(t, pool)
	defer teardown(t, pool)

	mustExec(t, pool, `CREATE TABLE _hc_test.vm_mismatch(id serial PRIMARY KEY, data text)`)
	mustExec(t, pool, `INSERT INTO _hc_test.vm_mismatch(data) SELECT repeat('y', 100) FROM generate_series(1, 500)`)
	// VACUUM sets relallvisible = relpages (legitimate state).
	mustExec(t, pool, `VACUUM _hc_test.vm_mismatch`)

	// Confirm relpages > 0 before we inject the mismatch.
	var relpages int
	if err := pool.QueryRow(context.Background(), //nolint:gosec // nosemgrep
		`SELECT relpages FROM pg_class WHERE oid = '_hc_test.vm_mismatch'::regclass`).Scan(&relpages); err != nil || relpages == 0 {
		t.Skipf("relpages=%d after VACUUM — cannot inject mismatch (err=%v)", relpages, err)
	}

	// Inject: set relallvisible to relpages + 20 (impossible, signals VM corruption).
	mustExec(t, pool,
		`UPDATE pg_class SET relallvisible = relpages + 20 WHERE oid = '_hc_test.vm_mismatch'::regclass`)
	t.Logf("injected: relpages=%d relallvisible=%d", relpages, relpages+20)

	results := runHealthcheck(t, "--groups", "G08")

	c, ok := results["G08-002"]
	if !ok {
		t.Fatal("G08-002 not found in output")
	}
	if c.Severity != "WARN" {
		t.Errorf("G08-002: expected WARN for relallvisible > relpages, got %s — %s", c.Severity, c.Observed)
	} else {
		t.Logf("G08-002: %s — %s ✓", c.Severity, c.Observed)
	}
}
