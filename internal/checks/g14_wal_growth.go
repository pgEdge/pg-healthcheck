package checks

// G14 — WAL Growth & Generation Rate
//
// The existing groups cover WAL from two angles:
//   G02 monitors the archiving pipeline (WAL leaving via pgBackRest).
//   G09 monitors replication slot retention (WAL held back by inactive consumers).
//
// G14 fills the gap: it watches WAL as a first-class resource on its own.
// It answers three questions:
//   1. How much WAL is on disk right now?
//   2. How fast is it being produced?
//   3. What is causing any abnormal rate?
//
// Together G02 + G09 + G14 give a complete WAL health picture.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgedge/pg-healthcheck/internal/config"
)

const g14 = "WAL Growth & Generation Rate"

// G14WALGrowth implements the Checker interface for Group 14.
type G14WALGrowth struct{}

func (g *G14WALGrowth) Name() string    { return g14 }
func (g *G14WALGrowth) GroupID() string { return "G14" }

// Run executes all G14 checks.
// LSN samples bookend the other checks so the two-sample WAL rate measurement
// naturally spans a real workload interval rather than requiring an extra sleep.
func (g *G14WALGrowth) Run(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) ([]Finding, error) {
	var f []Finding

	f = append(f, g14WALDirSize(ctx, db, cfg)...)
	f = append(f, g14EphemeralStateFile(cfg)...)

	// LSN sample 1 — taken before the middle checks
	lsn1, t1 := g14SampleLSN(ctx, db)

	f = append(f, g14WALStatSummary(ctx, db)...)
	f = append(f, g14FPIRate(ctx, db, cfg)...)
	f = append(f, g14TopWALRelations(ctx, db)...)
	f = append(f, g14WALCompression(ctx, db)...)
	f = append(f, g14WALLevelLogical(ctx, db)...)
	f = append(f, g14WALSegmentCount(ctx, db)...)
	f = append(f, g14UnarchivedAge(ctx, db)...)
	f = append(f, g14UnloggedTables(ctx, db)...)
	f = append(f, g14WALFilesystemPct(ctx, db, cfg)...)
	f = append(f, g14LongTxWALRetain(ctx, db)...)
	f = append(f, g14CheckpointForced(ctx, db)...)
	f = append(f, g14WALAccumulationSinceCheckpoint(ctx, db)...)

	// Ensure at least 1 second has elapsed since sample 1 so the rate
	// calculation has a meaningful denominator even on fast/quiet systems.
	if elapsed := time.Since(t1); elapsed < time.Second {
		time.Sleep(time.Second - elapsed)
	}

	// LSN sample 2 — taken after all other checks have run
	lsn2, t2 := g14SampleLSN(ctx, db)

	f = append(f, g14WALRate(lsn1, t1, lsn2, t2, cfg)...)

	return f, nil
}

// ── LSN sampling ──────────────────────────────────────────────────────────────

// g14SampleLSN returns the current WAL LSN as a uint64 and the server time.
func g14SampleLSN(ctx context.Context, db *pgxpool.Pool) (uint64, time.Time) {
	var lsnStr string
	var ts time.Time
	if err := db.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text, NOW()").Scan(&lsnStr, &ts); err != nil {
		return 0, time.Now()
	}
	var hi, lo uint64
	_, _ = fmt.Sscanf(lsnStr, "%X/%X", &hi, &lo)
	return hi<<32 | lo, ts
}

// ── G14-016  Ephemeral WAL rate baseline state file ───────────────────────────
//
// The WAL rate baseline (G14-003) is only as useful as the history behind it.
// When the state file lives in /tmp or /var/tmp it is cleared on every reboot,
// silently wiping the rolling average. Operators are then unaware that the
// baseline comparison in G14-003 is starting from scratch and will not flag
// abnormal WAL rates until enough new samples accumulate.
func g14EphemeralStateFile(cfg *config.Config) []Finding {
	const id = "G14-016"
	const name = "WAL rate state file persistence"

	stateFile := cfg.WALRateStateFile
	if stateFile == "" {
		stateFile = "/tmp/pg-healthcheck_wal_rate.json"
	}

	ephemeral := false
	for _, prefix := range []string{"/tmp/", "/tmp", "/var/tmp/", "/var/tmp"} {
		if strings.HasPrefix(stateFile, prefix) {
			ephemeral = true
			break
		}
	}
	if !ephemeral {
		return []Finding{NewOK(id, g14, name,
			fmt.Sprintf("State file is in a persistent location: %s", stateFile),
			"https://www.postgresql.org/docs/current/wal-configuration.html")}
	}

	return []Finding{NewWarn(id, g14, name,
		fmt.Sprintf("WAL rate state file is in an ephemeral path: %s", stateFile),
		"Set wal_rate_state_file in healthcheck.yaml to a persistent path, e.g. /var/lib/pg-healthcheck/wal_rate.json",
		"The rolling WAL baseline (G14-003) is reset on every system reboot because /tmp is cleared at boot. "+
			"After a reboot, G14-003 will report 'Collecting baseline' until enough samples accumulate again, "+
			"meaning abnormal WAL rates go undetected during that window.",
		"https://www.postgresql.org/docs/current/wal-configuration.html")}
}

// ── G14-001  pg_wal directory size ────────────────────────────────────────────

func g14WALDirSize(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	const q = `SELECT COALESCE(SUM(size), 0) FROM pg_ls_waldir()`
	var walBytes int64
	if err := db.QueryRow(ctx, q).Scan(&walBytes); err != nil {
		return []Finding{NewInfo("G14-001", g14, "pg_wal directory size",
			"pg_ls_waldir() inaccessible — grant pg_monitor role to the healthcheck user",
			"GRANT pg_monitor TO <healthcheck_user>;",
			"", "https://www.postgresql.org/docs/current/functions-admin.html")}
	}
	walGB := float64(walBytes) / (1024 * 1024 * 1024)
	obs := fmt.Sprintf("pg_wal size: %.2f GB", walGB)

	critGB := int64(cfg.WALDirCritGB)
	warnGB := int64(cfg.WALDirWarnGB)
	if critGB <= 0 {
		critGB = 50
	}
	if warnGB <= 0 {
		warnGB = 20
	}

	switch {
	case walBytes >= critGB*1024*1024*1024:
		return []Finding{NewCrit("G14-001", g14, "pg_wal directory size", obs,
			"Investigate WAL generation rate; check for inactive replication slots or long transactions.",
			fmt.Sprintf("pg_wal exceeds the %d GB critical threshold.", critGB),
			"https://www.postgresql.org/docs/current/wal-configuration.html")}
	case walBytes >= warnGB*1024*1024*1024:
		return []Finding{NewWarn("G14-001", g14, "pg_wal directory size", obs,
			"Monitor closely; investigate WAL growth rate and slot activity.",
			"", "https://www.postgresql.org/docs/current/wal-configuration.html")}
	}
	return []Finding{NewOK("G14-001", g14, "pg_wal directory size", obs,
		"https://www.postgresql.org/docs/current/wal-configuration.html")}
}

// ── G14-002 & G14-003  WAL generation rate + baseline comparison ──────────────

// walRateState persists rolling baseline samples between runs.
type walRateState struct {
	Samples    []walRateSample `json:"samples"`
	RollingAvg float64         `json:"rolling_avg_bytes_per_sec"`
}

type walRateSample struct {
	TS              string  `json:"ts"`
	RateBytesPerSec float64 `json:"rate_bytes_per_sec"`
}

// g14WALRate emits G14-002 (absolute rate) and G14-003 (vs rolling baseline).
func g14WALRate(lsn1 uint64, t1 time.Time, lsn2 uint64, t2 time.Time, cfg *config.Config) []Finding {
	if lsn1 == 0 || lsn2 == 0 || lsn2 < lsn1 {
		return []Finding{NewSkip("G14-002", g14, "WAL generation rate",
			"Could not obtain valid LSN samples")}
	}
	dur := t2.Sub(t1).Seconds()
	if dur < 0.5 {
		return []Finding{NewInfo("G14-002", g14, "WAL generation rate",
			fmt.Sprintf("Sampling interval %.2fs too short for a reliable measurement", dur),
			"", "", "")}
	}

	rateBS := float64(lsn2-lsn1) / dur
	rateMBS := rateBS / (1024 * 1024)
	obs := fmt.Sprintf("WAL rate: %.1f MB/s  (%.0f bytes over %.1fs)", rateMBS, float64(lsn2-lsn1), dur)

	warnMBS := float64(cfg.WALRateWarnMBs)
	critMBS := float64(cfg.WALRateCritMBs)
	if warnMBS <= 0 {
		warnMBS = 50
	}
	if critMBS <= 0 {
		critMBS = 200
	}

	var findings []Finding

	// G14-002 — absolute rate against configured thresholds
	switch {
	case rateMBS >= critMBS:
		findings = append(findings, NewCrit("G14-002", g14, "WAL generation rate", obs,
			"Identify top WAL-generating tables; look for bulk writes or FPI storms.",
			"", "https://www.postgresql.org/docs/current/wal-configuration.html"))
	case rateMBS >= warnMBS:
		findings = append(findings, NewWarn("G14-002", g14, "WAL generation rate", obs,
			"Monitor for sustained high WAL generation.",
			"", "https://www.postgresql.org/docs/current/wal-configuration.html"))
	default:
		findings = append(findings, NewOK("G14-002", g14, "WAL generation rate", obs,
			"https://www.postgresql.org/docs/current/wal-configuration.html"))
	}

	// G14-003 — compare against rolling baseline stored in state file
	rollingAvg := g14UpdateStateFile(cfg, rateBS)
	if rollingAvg <= 0 {
		findings = append(findings, NewInfo("G14-003", g14, "WAL rate vs rolling baseline",
			"Collecting baseline — will compare once 2+ samples are stored",
			"Run pg-healthcheck regularly to build a rolling baseline.",
			"", ""))
	} else {
		mult := cfg.WALRateBaselineMultiplier
		if mult <= 0 {
			mult = 3.0
		}
		bsObs := fmt.Sprintf("%.1f MB/s now  vs  %.1f MB/s avg  (threshold: %.1fx)",
			rateMBS, rollingAvg/(1024*1024), mult)
		if rateBS > rollingAvg*mult {
			findings = append(findings, NewWarn("G14-003", g14, "WAL rate vs rolling baseline", bsObs,
				"Rate is significantly above baseline — investigate for bulk operations or config changes.",
				"", "https://www.postgresql.org/docs/current/wal-configuration.html"))
		} else {
			findings = append(findings, NewOK("G14-003", g14, "WAL rate vs rolling baseline", bsObs,
				"https://www.postgresql.org/docs/current/wal-configuration.html"))
		}
	}

	return findings
}

// g14UpdateStateFile reads the JSON state file, appends the new sample,
// trims to the configured window, writes back atomically, and returns the
// new rolling average (0 if fewer than 2 samples exist).
func g14UpdateStateFile(cfg *config.Config, rateBS float64) float64 {
	stateFile := cfg.WALRateStateFile
	if stateFile == "" {
		stateFile = "/tmp/pg-healthcheck_wal_rate.json"
	}

	var state walRateState
	if data, err := os.ReadFile(stateFile); err == nil {
		_ = json.Unmarshal(data, &state)
	}

	maxSamples := cfg.WALRateBaselineSamples
	if maxSamples <= 0 {
		maxSamples = 12
	}
	state.Samples = append(state.Samples, walRateSample{
		TS:              time.Now().UTC().Format(time.RFC3339),
		RateBytesPerSec: rateBS,
	})
	if len(state.Samples) > maxSamples {
		state.Samples = state.Samples[len(state.Samples)-maxSamples:]
	}

	var sum float64
	for _, s := range state.Samples {
		sum += s.RateBytesPerSec
	}
	state.RollingAvg = sum / float64(len(state.Samples))

	// Atomic write: write to .tmp then rename
	if data, err := json.MarshalIndent(state, "", "  "); err == nil {
		tmp := stateFile + ".tmp"
		if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err == nil {
			if err := os.WriteFile(tmp, data, 0o644); err == nil {
				_ = os.Rename(tmp, stateFile)
			}
		}
	}

	if len(state.Samples) < 2 {
		return 0
	}
	return state.RollingAvg
}

// ── G14-004  WAL statistics summary (pg_stat_wal, PG 14+) ────────────────────

func g14WALStatSummary(ctx context.Context, db *pgxpool.Pool) []Finding {
	var major int
	if err := db.QueryRow(ctx, "SELECT current_setting('server_version_num')::int / 10000").Scan(&major); err != nil {
		return []Finding{NewSkip("G14-004", g14, "WAL statistics summary", err.Error())}
	}
	if major < 14 {
		return []Finding{NewInfo("G14-004", g14, "WAL statistics summary",
			fmt.Sprintf("PostgreSQL %d — pg_stat_wal is available in PG 14+", major),
			"Upgrade to PG 14+ for detailed WAL statistics.",
			"", "https://www.postgresql.org/docs/current/monitoring-stats.html")}
	}
	const q = `SELECT wal_bytes, wal_records, wal_fpi, wal_buffers_full,
	                   COALESCE(EXTRACT(EPOCH FROM (NOW() - stats_reset))::bigint, 0)
	            FROM pg_stat_wal`
	var walBytes, walRecords, walFPI, walBufFull, ageSecs int64
	if err := db.QueryRow(ctx, q).Scan(&walBytes, &walRecords, &walFPI, &walBufFull, &ageSecs); err != nil {
		return []Finding{NewSkip("G14-004", g14, "WAL statistics summary", err.Error())}
	}
	obs := fmt.Sprintf("%.1f GB WAL | %d records | %d FPI | %d buf_full  (stats since %s ago)",
		float64(walBytes)/1024/1024/1024, walRecords, walFPI, walBufFull, g14FmtSecs(ageSecs))
	if walBufFull > 10000 {
		return []Finding{NewWarn("G14-004", g14, "WAL statistics summary", obs,
			"Increase wal_buffers to reduce WAL buffer contention.",
			"wal_buffers_full means WAL buffers are being flushed due to insufficient buffer space.",
			"https://www.postgresql.org/docs/current/runtime-config-wal.html")}
	}
	return []Finding{NewOK("G14-004", g14, "WAL statistics summary", obs,
		"https://www.postgresql.org/docs/current/monitoring-stats.html")}
}

// ── G14-005  Full-page write (FPI) ratio ─────────────────────────────────────

func g14FPIRate(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	var major int
	if err := db.QueryRow(ctx, "SELECT current_setting('server_version_num')::int / 10000").Scan(&major); err != nil {
		return []Finding{NewSkip("G14-005", g14, "Full-page write ratio", err.Error())}
	}
	if major < 14 {
		return []Finding{NewInfo("G14-005", g14, "Full-page write ratio",
			fmt.Sprintf("PostgreSQL %d — pg_stat_wal available in PG 14+", major),
			"", "", "https://www.postgresql.org/docs/current/monitoring-stats.html")}
	}
	const q = `SELECT wal_bytes, wal_records, wal_fpi FROM pg_stat_wal`
	var walBytes, walRecords, walFPI int64
	if err := db.QueryRow(ctx, q).Scan(&walBytes, &walRecords, &walFPI); err != nil {
		return []Finding{NewSkip("G14-005", g14, "Full-page write ratio", err.Error())}
	}
	if walRecords == 0 {
		return []Finding{NewOK("G14-005", g14, "Full-page write ratio",
			"No WAL records yet", "https://www.postgresql.org/docs/current/monitoring-stats.html")}
	}
	ratio := float64(walFPI) / float64(walRecords)
	obs := fmt.Sprintf("FPI ratio: %.1f%%  (%d FPI out of %d total records)", ratio*100, walFPI, walRecords)

	threshold := cfg.WALFPIRatioWarn
	if threshold <= 0 {
		threshold = 0.40
	}
	if ratio > threshold {
		return []Finding{NewWarn("G14-005", g14, "Full-page write ratio", obs,
			"Increase shared_buffers to reduce post-checkpoint FPI storms; check checkpoint frequency.",
			"High FPI ratio means every first-write after a checkpoint records a full 8 kB page.",
			"https://www.postgresql.org/docs/current/wal-configuration.html")}
	}
	return []Finding{NewOK("G14-005", g14, "Full-page write ratio", obs,
		"https://www.postgresql.org/docs/current/wal-configuration.html")}
}

// ── G14-006  Top WAL-generating tables ───────────────────────────────────────

func g14TopWALRelations(ctx context.Context, db *pgxpool.Pool) []Finding {
	const q = `SELECT schemaname || '.' || relname,
	                   n_tup_ins + n_tup_upd + n_tup_del + n_tup_hot_upd AS mods
	            FROM pg_stat_user_tables
	            ORDER BY mods DESC LIMIT 5`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G14-006", g14, "Top WAL-generating tables", err.Error())}
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var tbl string
		var mods int64
		_ = rows.Scan(&tbl, &mods)
		lines = append(lines, fmt.Sprintf("%-50s  %d modifications", tbl, mods))
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G14-006", g14, "Top WAL-generating tables", "scan error: "+err.Error())}
	}
	if len(lines) == 0 {
		return []Finding{NewOK("G14-006", g14, "Top WAL-generating tables",
			"No table statistics available yet",
			"https://www.postgresql.org/docs/current/monitoring-stats.html")}
	}
	return []Finding{NewInfo("G14-006", g14, "Top WAL-generating tables",
		lines[0],
		"Review high-modification tables for bulk write patterns.",
		strings.Join(lines, "\n"),
		"https://www.postgresql.org/docs/current/monitoring-stats.html")}
}

// ── G14-007  WAL compression advisory ────────────────────────────────────────

func g14WALCompression(ctx context.Context, db *pgxpool.Pool) []Finding {
	var val string
	if err := db.QueryRow(ctx, "SELECT setting FROM pg_settings WHERE name='wal_compression'").Scan(&val); err != nil {
		return []Finding{NewSkip("G14-007", g14, "WAL compression", err.Error())}
	}
	obs := fmt.Sprintf("wal_compression = %s", val)
	if val == "off" || val == "false" {
		return []Finding{NewInfo("G14-007", g14, "WAL compression", obs,
			"Consider enabling wal_compression=on — reduces FPI size with a slight CPU overhead.",
			"wal_compression=on compresses full-page-write images, significantly reducing WAL volume on compressible data.",
			"https://www.postgresql.org/docs/current/runtime-config-wal.html")}
	}
	return []Finding{NewOK("G14-007", g14, "WAL compression", obs,
		"https://www.postgresql.org/docs/current/runtime-config-wal.html")}
}

// ── G14-008  wal_level=logical when not needed ───────────────────────────────

func g14WALLevelLogical(ctx context.Context, db *pgxpool.Pool) []Finding {
	var walLevel string
	if err := db.QueryRow(ctx, "SELECT setting FROM pg_settings WHERE name='wal_level'").Scan(&walLevel); err != nil {
		return []Finding{NewSkip("G14-008", g14, "Unnecessary wal_level=logical", err.Error())}
	}
	if walLevel != "logical" {
		return []Finding{NewOK("G14-008", g14, "Unnecessary wal_level=logical",
			fmt.Sprintf("wal_level = %s", walLevel),
			"https://www.postgresql.org/docs/current/runtime-config-wal.html")}
	}

	// Condition 2: active logical replication slots?
	var slotCnt int
	_ = db.QueryRow(ctx, "SELECT COUNT(*) FROM pg_replication_slots WHERE slot_type='logical'").Scan(&slotCnt)
	if slotCnt > 0 {
		return []Finding{NewOK("G14-008", g14, "Unnecessary wal_level=logical",
			fmt.Sprintf("wal_level=logical with %d active logical slot(s) — justified", slotCnt),
			"https://www.postgresql.org/docs/current/runtime-config-wal.html")}
	}

	// Condition 3: Spock subscriptions present?
	var spockTbl int
	_ = db.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.tables
	    WHERE table_schema='spock' AND table_name='subscription'`).Scan(&spockTbl)
	if spockTbl > 0 {
		var subCnt int
		_ = db.QueryRow(ctx, "SELECT COUNT(*) FROM spock.subscription").Scan(&subCnt)
		if subCnt > 0 {
			return []Finding{NewOK("G14-008", g14, "Unnecessary wal_level=logical",
				fmt.Sprintf("wal_level=logical with %d Spock subscription(s) — justified", subCnt),
				"https://www.postgresql.org/docs/current/runtime-config-wal.html")}
		}
	}

	// All three conditions met: wal_level=logical with nothing using it
	return []Finding{NewWarn("G14-008", g14, "Unnecessary wal_level=logical",
		"wal_level=logical but no logical replication slots or Spock subscriptions found",
		"Set wal_level=replica to reduce WAL volume — requires a PostgreSQL restart.",
		"logical WAL is materially larger than replica WAL for the same write workload.",
		"https://www.postgresql.org/docs/current/runtime-config-wal.html")}
}

// ── G14-009  WAL segment count ───────────────────────────────────────────────

func g14WALSegmentCount(ctx context.Context, db *pgxpool.Pool) []Finding {
	// Count only WAL segment files (24-character hex names); exclude .history etc.
	const q = `SELECT COUNT(*) FROM pg_ls_waldir() WHERE name ~ '^[0-9A-F]{24}$'`
	var cnt int64
	if err := db.QueryRow(ctx, q).Scan(&cnt); err != nil {
		return []Finding{NewInfo("G14-009", g14, "WAL segment count",
			"pg_ls_waldir() not accessible",
			"Grant pg_monitor role to the healthcheck user.",
			"", "")}
	}
	obs := fmt.Sprintf("%d WAL segment file(s) present", cnt)
	if cnt > 1000 {
		return []Finding{NewWarn("G14-009", g14, "WAL segment count", obs,
			"Check for inactive replication slots or long transactions blocking WAL recycling.",
			"A high segment count means WAL segments cannot be recycled as expected.",
			"https://www.postgresql.org/docs/current/wal-configuration.html")}
	}
	return []Finding{NewOK("G14-009", g14, "WAL segment count", obs,
		"https://www.postgresql.org/docs/current/wal-configuration.html")}
}

// ── G14-010  Unarchived WAL age ───────────────────────────────────────────────

func g14UnarchivedAge(ctx context.Context, db *pgxpool.Pool) []Finding {
	// Use last_failed_time recency instead of cumulative failed_count so that old,
	// resolved failures do not produce perpetual alerts.
	const q = `SELECT
		COALESCE(EXTRACT(EPOCH FROM (NOW() - last_archived_time))::bigint, -1) AS last_ok_secs,
		archived_count,
		failed_count,
		COALESCE(EXTRACT(EPOCH FROM (NOW() - last_failed_time))::bigint, -1) AS last_fail_secs
		FROM pg_stat_archiver`
	var lastOKSecs, archived, failed, lastFailSecs int64
	if err := db.QueryRow(ctx, q).Scan(&lastOKSecs, &archived, &failed, &lastFailSecs); err != nil {
		return []Finding{NewSkip("G14-010", g14, "WAL archiver status", err.Error())}
	}
	if archived == 0 && failed == 0 {
		return []Finding{NewInfo("G14-010", g14, "WAL archiver status",
			"Archiving not yet started or archive_mode=off",
			"", "", "https://www.postgresql.org/docs/current/continuous-archiving.html")}
	}
	obs := fmt.Sprintf("Last archive: %s ago | archived: %d | failed: %d",
		g14FmtSecs(lastOKSecs), archived, failed)
	// Alert only on recent failures — ignore old resolved ones (lastFailSecs == -1 means never failed).
	if lastFailSecs >= 0 && lastFailSecs < 3600 {
		return []Finding{NewCrit("G14-010", g14, "WAL archiver status", obs,
			"Fix archiver errors immediately — unarchived WAL fills pg_wal and can crash the server.",
			fmt.Sprintf("Archive failure within the last %s; last_failed_time is recent", g14FmtSecs(lastFailSecs)),
			"https://www.postgresql.org/docs/current/continuous-archiving.html")}
	}
	if lastFailSecs >= 0 && lastFailSecs < 86400 {
		return []Finding{NewWarn("G14-010", g14, "WAL archiver status", obs,
			"Archive failure recorded in the last 24 hours — verify the archiver has fully recovered.",
			fmt.Sprintf("Last failure was %s ago (cumulative failures: %d)", g14FmtSecs(lastFailSecs), failed),
			"https://www.postgresql.org/docs/current/continuous-archiving.html")}
	}
	if lastOKSecs > 3600 {
		return []Finding{NewWarn("G14-010", g14, "WAL archiver status", obs,
			"Last successful archive was >1 hour ago — investigate the archiver process.",
			"", "https://www.postgresql.org/docs/current/continuous-archiving.html")}
	}
	return []Finding{NewOK("G14-010", g14, "WAL archiver status", obs,
		"https://www.postgresql.org/docs/current/continuous-archiving.html")}
}

// ── G14-011  UNLOGGED tables advisory ────────────────────────────────────────

func g14UnloggedTables(ctx context.Context, db *pgxpool.Pool) []Finding {
	const q = `SELECT n.nspname || '.' || c.relname
	            FROM pg_class c
	            JOIN pg_namespace n ON n.oid = c.relnamespace
	            WHERE c.relkind = 'r'
	              AND c.relpersistence = 'u'
	              AND n.nspname NOT IN ('pg_catalog','information_schema')
	            ORDER BY 1 LIMIT 20`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G14-011", g14, "UNLOGGED tables advisory", err.Error())}
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var t string
		_ = rows.Scan(&t)
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G14-011", g14, "UNLOGGED tables advisory", "scan error: "+err.Error())}
	}
	if len(tables) == 0 {
		return []Finding{NewOK("G14-011", g14, "UNLOGGED tables advisory",
			"No UNLOGGED tables found",
			"https://www.postgresql.org/docs/current/sql-createtable.html")}
	}
	obs := fmt.Sprintf("%d UNLOGGED table(s) present", len(tables))
	return []Finding{NewInfo("G14-011", g14, "UNLOGGED tables advisory", obs,
		"Plan carefully before converting UNLOGGED → LOGGED — it triggers a large WAL spike.",
		strings.Join(tables, "\n"),
		"https://www.postgresql.org/docs/current/sql-createtable.html")}
}

// ── G14-012  Forced checkpoint rate ──────────────────────────────────────────

func g14CheckpointForced(ctx context.Context, db *pgxpool.Pool) []Finding {
	const q = `SELECT checkpoints_req, checkpoints_timed,
	                   CASE WHEN checkpoints_req + checkpoints_timed = 0 THEN 0
	                        ELSE checkpoints_req * 100 / (checkpoints_req + checkpoints_timed)
	                   END AS req_pct
	            FROM pg_stat_bgwriter`
	var req, timed, reqPct int64
	if err := db.QueryRow(ctx, q).Scan(&req, &timed, &reqPct); err != nil {
		return []Finding{NewSkip("G14-012", g14, "Forced checkpoint rate", err.Error())}
	}
	if req+timed == 0 {
		return []Finding{NewOK("G14-012", g14, "Forced checkpoint rate",
			"No checkpoints completed yet",
			"https://www.postgresql.org/docs/current/runtime-config-wal.html")}
	}
	obs := fmt.Sprintf("Forced: %d  Scheduled: %d  (%d%% forced)", req, timed, reqPct)
	if reqPct > 20 {
		return []Finding{NewWarn("G14-012", g14, "Forced checkpoint rate", obs,
			"Increase max_wal_size so checkpoints complete on their schedule rather than being forced.",
			"checkpoints_req > 20% of total checkpoints means max_wal_size is too small for the workload.",
			"https://www.postgresql.org/docs/current/runtime-config-wal.html")}
	}
	return []Finding{NewOK("G14-012", g14, "Forced checkpoint rate", obs,
		"https://www.postgresql.org/docs/current/runtime-config-wal.html")}
}

// ── G14-013  pg_wal filesystem percentage ────────────────────────────────────

func g14WALFilesystemPct(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	var dataDir string
	if err := db.QueryRow(ctx, "SELECT setting FROM pg_settings WHERE name='data_directory'").Scan(&dataDir); err != nil {
		return []Finding{NewSkip("G14-013", g14, "pg_wal filesystem usage", err.Error())}
	}
	walPath := filepath.Join(dataDir, "pg_wal")

	pct, totalGB, freeGB, err := diskFreeStats(walPath)
	if err != nil {
		if os.IsPermission(err) {
			return []Finding{NewInfo("G14-013", g14, "pg_wal filesystem usage",
				fmt.Sprintf("pg_wal filesystem check: permission denied (path: %s)", walPath),
				"Grant the OS user running pg-healthcheck read access to the data directory, or run as the postgres user.",
				err.Error(),
				"https://www.postgresql.org/docs/current/wal-configuration.html")}
		}
		return []Finding{NewInfo("G14-013", g14, "pg_wal filesystem usage",
			fmt.Sprintf("pg_wal filesystem check requires local execution (path: %s)", walPath),
			"Run pg-healthcheck directly on the PostgreSQL host — not via a remote connection.",
			err.Error(),
			"https://www.postgresql.org/docs/current/wal-configuration.html")}
	}
	obs := fmt.Sprintf("pg_wal filesystem %d%% used  (%.1f GB free of %.1f GB total)",
		pct, freeGB, totalGB)

	warnPct := cfg.WALFilesystemWarnPct
	critPct := cfg.WALFilesystemCritPct
	if warnPct <= 0 {
		warnPct = 60
	}
	if critPct <= 0 {
		critPct = 80
	}

	switch {
	case pct >= critPct:
		return []Finding{NewCrit("G14-013", g14, "pg_wal filesystem usage", obs,
			"IMMEDIATE: pg_wal exhaustion crashes PostgreSQL — reduce WAL generation or expand the filesystem now.",
			"", "https://www.postgresql.org/docs/current/wal-configuration.html")}
	case pct >= warnPct:
		return []Finding{NewWarn("G14-013", g14, "pg_wal filesystem usage", obs,
			"Monitor closely — investigate WAL growth rate before the filesystem fills.",
			"", "https://www.postgresql.org/docs/current/wal-configuration.html")}
	}
	return []Finding{NewOK("G14-013", g14, "pg_wal filesystem usage", obs,
		"https://www.postgresql.org/docs/current/wal-configuration.html")}
}

// ── G14-014  Long transactions blocking WAL segment recycling ─────────────────

func g14LongTxWALRetain(ctx context.Context, db *pgxpool.Pool) []Finding {
	// Any open transaction whose start LSN predates the current WAL position
	// prevents PostgreSQL from recycling older WAL segments.
	// We flag transactions open for more than 5 minutes.
	const q = `SELECT pid, usename, application_name, state,
	                   EXTRACT(EPOCH FROM (now() - xact_start))::bigint AS age_secs
	            FROM pg_stat_activity
	            WHERE xact_start IS NOT NULL
	              AND now() - xact_start > interval '5 minutes'
	            ORDER BY age_secs DESC
	            LIMIT 10`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G14-014", g14, "Long transactions blocking WAL recycle", err.Error())}
	}
	defer rows.Close()

	type txRow struct {
		pid, ageSecs     int64
		user, app, state string
	}
	var txs []txRow
	for rows.Next() {
		var t txRow
		_ = rows.Scan(&t.pid, &t.user, &t.app, &t.state, &t.ageSecs)
		txs = append(txs, t)
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G14-014", g14, "Long transactions blocking WAL recycle", "scan error: "+err.Error())}
	}

	if len(txs) == 0 {
		return []Finding{NewOK("G14-014", g14, "Long transactions blocking WAL recycle",
			"No transactions open longer than 5 minutes",
			"https://www.postgresql.org/docs/current/wal-configuration.html")}
	}

	oldest := txs[0]
	obs := fmt.Sprintf("%d transaction(s) open >5min  |  oldest: pid %d (%s) for %s",
		len(txs), oldest.pid, oldest.user, g14FmtSecs(oldest.ageSecs))

	var detail []string
	for _, t := range txs {
		detail = append(detail, fmt.Sprintf("pid %-6d  user=%-15s  app=%-20s  state=%-12s  open=%s",
			t.pid, t.user, t.app, t.state, g14FmtSecs(t.ageSecs)))
	}
	return []Finding{NewWarn("G14-014", g14, "Long transactions blocking WAL recycle", obs,
		"Terminate long-idle transactions; set idle_in_transaction_session_timeout to auto-clean.",
		strings.Join(detail, "\n"),
		"https://www.postgresql.org/docs/current/wal-configuration.html")}
}

// ── G14-015  WAL accumulation since last checkpoint ───────────────────────────
//
// PostgreSQL can only recycle WAL segments that are older than the last
// checkpoint LSN. Until a checkpoint runs, every segment generated since the
// previous checkpoint is "reserved" on disk — it cannot be removed or reused
// even if its data has already been replicated and archived.
//
// On write-heavy nodes in a Spock cluster this can silently fill the pg_wal
// directory: write traffic builds up WAL on n2/n3 while n1 only receives;
// if checkpoint_timeout is long (e.g. 15 min) and max_wal_size is small,
// the unreclaimable segment pile grows until disk pressure forces a checkpoint
// or an operator runs one manually.
//
// This check compares bytes generated since the last checkpoint against
// max_wal_size to give early warning before the situation becomes critical.
// When this fires on some cluster nodes but not others it is a strong signal
// that write traffic is imbalanced across the cluster.
func g14WALAccumulationSinceCheckpoint(ctx context.Context, db *pgxpool.Pool) []Finding {
	const q = `
		SELECT
		    pg_wal_lsn_diff(pg_current_wal_lsn(), checkpoint_lsn)         AS bytes_since_ckpt,
		    EXTRACT(EPOCH FROM (now() - checkpoint_time))::bigint           AS secs_since_ckpt,
		    (SELECT setting::bigint * 1024 * 1024
		     FROM pg_settings WHERE name = 'max_wal_size')                  AS max_wal_bytes
		FROM pg_control_checkpoint()`
	var bytesSinceCkpt, secsSinceCkpt, maxWALBytes int64
	if err := db.QueryRow(ctx, q).Scan(&bytesSinceCkpt, &secsSinceCkpt, &maxWALBytes); err != nil {
		return []Finding{NewSkip("G14-015", g14, "WAL accumulation since checkpoint", err.Error())}
	}
	if maxWALBytes <= 0 {
		return []Finding{NewSkip("G14-015", g14, "WAL accumulation since checkpoint",
			"Could not read max_wal_size from pg_settings")}
	}

	pct := int(float64(bytesSinceCkpt) / float64(maxWALBytes) * 100)
	obs := fmt.Sprintf(
		"%d%% of max_wal_size used since last checkpoint  (%.1f GB unreclaimable / %.1f GB max_wal_size — checkpoint %s ago)",
		pct,
		float64(bytesSinceCkpt)/1024/1024/1024,
		float64(maxWALBytes)/1024/1024/1024,
		g14FmtSecs(secsSinceCkpt),
	)

	if pct >= 80 {
		return []Finding{NewCrit("G14-015", g14, "WAL accumulation since checkpoint", obs,
			"Run CHECKPOINT immediately on this node, then increase max_wal_size or reduce checkpoint_timeout.",
			fmt.Sprintf(
				"%.1f GB of WAL segments are reserved and cannot be recycled until the next checkpoint. "+
					"At this rate a forced checkpoint is imminent, which causes an I/O spike. "+
					"On a Spock cluster, check whether write traffic is balanced — this warning "+
					"firing only on certain nodes indicates write-traffic imbalance. "+
					"Resolution: run 'CHECKPOINT;' on affected nodes, then tune max_wal_size "+
					"(currently %.1f GB) to match your workload.",
				float64(bytesSinceCkpt)/1024/1024/1024,
				float64(maxWALBytes)/1024/1024/1024),
			"https://www.postgresql.org/docs/current/wal-configuration.html")}
	}
	if pct >= 50 {
		return []Finding{NewWarn("G14-015", g14, "WAL accumulation since checkpoint", obs,
			"Consider increasing max_wal_size or reducing checkpoint_timeout to spread checkpoint I/O.",
			fmt.Sprintf(
				"More than half of max_wal_size (%.1f GB) has accumulated since the last checkpoint "+
					"and cannot be recycled until the next one. On a Spock cluster this check "+
					"firing on write-heavy nodes (e.g. n2/n3) but not read nodes (e.g. n1) "+
					"indicates write-traffic imbalance — run CHECKPOINT on the affected nodes "+
					"to drain reserved segments and monitor whether the imbalance persists.",
				float64(maxWALBytes)/1024/1024/1024),
			"https://www.postgresql.org/docs/current/wal-configuration.html")}
	}
	return []Finding{NewOK("G14-015", g14, "WAL accumulation since checkpoint", obs,
		"https://www.postgresql.org/docs/current/wal-configuration.html")}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// g14FmtSecs formats a duration in seconds as a human-readable string.
func g14FmtSecs(secs int64) string {
	if secs < 0 {
		return "n/a"
	}
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	if secs < 3600 {
		return fmt.Sprintf("%dm", secs/60)
	}
	if secs < 86400 {
		return fmt.Sprintf("%dh%dm", secs/3600, (secs%3600)/60)
	}
	return fmt.Sprintf("%dd%dh", secs/86400, (secs%86400)/3600)
}
