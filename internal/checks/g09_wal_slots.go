package checks

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgedge/pg-healthcheck/internal/config"
)

const g09 = "WAL & Replication Slot Health"

// G09WALSlots checks WAL replication slot health.
type G09WALSlots struct{}

func (g *G09WALSlots) Name() string    { return g09 }
func (g *G09WALSlots) GroupID() string { return "G09" }

func (g *G09WALSlots) Run(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) ([]Finding, error) {
	var f []Finding
	f = append(f, g09InactiveSlots(ctx, db, cfg)...)
	f = append(f, g09RetainedWALPerSlot(ctx, db, cfg)...)
	f = append(f, g09SlotCount(ctx, db)...)
	f = append(f, g09ReplLagBytes(ctx, db, cfg)...)
	f = append(f, g09UnnamedStandbys(ctx, db)...)
	f = append(f, g09RecoveryMinApplyDelay(ctx, db)...)
	f = append(f, g09WALKeepSize(ctx, db)...)
	f = append(f, g09CrossRefG02(ctx, db)...)
	// Logical replication checks
	f = append(f, g09InvalidatedSlots(ctx, db)...)
	f = append(f, g09MaxSlotWALKeepSize(ctx, db)...)
	f = append(f, g09InactiveLogicalSlots(ctx, db, cfg)...)
	f = append(f, g09LogicalWorkerStatus(ctx, db)...)
	f = append(f, g09SubscriptionRelState(ctx, db)...)
	f = append(f, g09StreamingLagTime(ctx, db)...)
	f = append(f, g09LogicalDecodingWorkMem(ctx, db)...)
	f = append(f, g09OrphanedReplicationOrigins(ctx, db)...)
	return f, nil
}

// G09-001 inactive slots lag
// Physical slots use restart_lsn; logical slots use confirmed_flush_lsn.
// Slots created with immediately_reserve=false have both LSNs NULL until a standby connects —
// they are still dangerous (they retain WAL once a standby eventually connects) and warrant INFO.
func g09InactiveSlots(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	const q = `SELECT slot_name,
		(COALESCE(confirmed_flush_lsn, restart_lsn) IS NULL) AS lsn_null,
		COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), COALESCE(confirmed_flush_lsn, restart_lsn)), 0) AS lag_bytes
		FROM pg_replication_slots
		WHERE active = false
		ORDER BY lag_bytes DESC LIMIT 10`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G09-001", g09, "Inactive replication slot lag", err.Error())}
	}
	defer rows.Close()
	var critLines, warnLines, infoLines []string
	for rows.Next() {
		var slot string
		var lsnNull bool
		var lagBytes int64
		if err := rows.Scan(&slot, &lsnNull, &lagBytes); err != nil {
			continue
		}
		var line string
		if lsnNull {
			line = fmt.Sprintf("%s: WAL position not yet reserved", slot)
		} else {
			line = fmt.Sprintf("%s: lag=%dMB", slot, lagBytes/1024/1024)
		}
		if !lsnNull && lagBytes >= cfg.ReplLagCritBytes {
			critLines = append(critLines, line)
		} else if !lsnNull && lagBytes >= cfg.ReplLagWarnBytes {
			warnLines = append(warnLines, line)
		} else {
			infoLines = append(infoLines, line)
		}
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G09-001", g09, "Inactive replication slot lag", "scan error: "+err.Error())}
	}
	var findings []Finding
	if len(critLines) > 0 {
		findings = append(findings, NewCrit("G09-001", g09, "Inactive replication slot lag",
			fmt.Sprintf("%d inactive slot(s) at critical lag", len(critLines)),
			"Drop unused slots or reconnect consumers to prevent WAL disk exhaustion.",
			strings.Join(critLines, "\n"),
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html"))
	}
	if len(warnLines) > 0 {
		findings = append(findings, NewWarn("G09-001", g09, "Inactive replication slot lag",
			fmt.Sprintf("%d inactive slot(s) at warning lag", len(warnLines)),
			"Investigate why consumers are inactive; consider dropping unused slots.",
			strings.Join(warnLines, "\n"),
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html"))
	}
	if len(infoLines) > 0 {
		findings = append(findings, NewInfo("G09-001", g09, "Inactive replication slot lag",
			fmt.Sprintf("%d inactive slot(s) with low or unknown lag", len(infoLines)),
			"Verify these slots have active consumers; drop if unused.",
			strings.Join(infoLines, "\n"),
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html"))
	}
	if len(findings) == 0 {
		return []Finding{NewOK("G09-001", g09, "Inactive replication slot lag",
			"No inactive replication slots",
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html")}
	}
	return findings
}

// G09-002 retained WAL per slot in GB
func g09RetainedWALPerSlot(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	const q = `SELECT slot_name, slot_type,
		pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) AS retained_bytes
		FROM pg_replication_slots
		ORDER BY retained_bytes DESC LIMIT 10`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G09-002", g09, "Retained WAL per slot", err.Error())}
	}
	defer rows.Close()
	var critLines, warnLines []string
	for rows.Next() {
		var slot, slotType string
		var retainedBytes int64
		_ = rows.Scan(&slot, &slotType, &retainedBytes)
		retainedGB := retainedBytes / 1024 / 1024 / 1024
		line := fmt.Sprintf("%s (%s): %dGB retained", slot, slotType, retainedGB)
		if retainedGB >= int64(cfg.WALSlotCritGB) {
			critLines = append(critLines, line)
		} else if retainedGB >= int64(cfg.WALSlotWarnGB) {
			warnLines = append(warnLines, line)
		}
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G09-002", g09, "Retained WAL per slot", "scan error: "+err.Error())}
	}
	var findings []Finding
	if len(critLines) > 0 {
		findings = append(findings, NewCrit("G09-002", g09, "Retained WAL per slot",
			fmt.Sprintf("%d slot(s) retaining >= %dGB of WAL", len(critLines), cfg.WALSlotCritGB),
			"Drop or catch up lagging slots to prevent WAL directory from filling disk.",
			strings.Join(critLines, "\n"),
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html"))
	}
	if len(warnLines) > 0 {
		findings = append(findings, NewWarn("G09-002", g09, "Retained WAL per slot",
			fmt.Sprintf("%d slot(s) retaining >= %dGB of WAL", len(warnLines), cfg.WALSlotWarnGB),
			"Monitor slot consumers; consider wal_receiver_timeout.",
			strings.Join(warnLines, "\n"),
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html"))
	}
	if len(findings) == 0 {
		return []Finding{NewOK("G09-002", g09, "Retained WAL per slot",
			fmt.Sprintf("All slots retain < %dGB of WAL", cfg.WALSlotWarnGB),
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html")}
	}
	return findings
}

// G09-003 slot count > 80% of max_replication_slots
func g09SlotCount(ctx context.Context, db *pgxpool.Pool) []Finding {
	var slotCount, maxSlots int
	_ = db.QueryRow(ctx, "SELECT count(*) FROM pg_replication_slots").Scan(&slotCount)
	_ = db.QueryRow(ctx, "SELECT setting::int FROM pg_settings WHERE name='max_replication_slots'").Scan(&maxSlots)
	if maxSlots == 0 {
		return []Finding{NewSkip("G09-003", g09, "Replication slot count",
			"max_replication_slots = 0")}
	}
	pct := slotCount * 100 / maxSlots
	obs := fmt.Sprintf("%d/%d slots used (%d%%)", slotCount, maxSlots, pct)
	if pct >= 80 {
		return []Finding{NewWarn("G09-003", g09, "Replication slot count", obs,
			fmt.Sprintf("Increase max_replication_slots or remove unused slots (currently %d/%d).", slotCount, maxSlots),
			"",
			"https://www.postgresql.org/docs/current/runtime-config-replication.html")}
	}
	return []Finding{NewOK("G09-003", g09, "Replication slot count", obs,
		"https://www.postgresql.org/docs/current/runtime-config-replication.html")}
}

// G09-004 pg_stat_replication lag bytes
func g09ReplLagBytes(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	const q = `SELECT application_name,
		pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) AS lag_bytes,
		state
		FROM pg_stat_replication
		ORDER BY lag_bytes DESC NULLS LAST LIMIT 10`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G09-004", g09, "Replication lag (pg_stat_replication)", err.Error())}
	}
	defer rows.Close()
	var critLines, warnLines []string
	var anyReplica bool
	for rows.Next() {
		anyReplica = true
		var appName, state string
		var lagBytes int64
		_ = rows.Scan(&appName, &lagBytes, &state)
		line := fmt.Sprintf("%s (%s): lag=%dMB", appName, state, lagBytes/1024/1024)
		if lagBytes >= cfg.ReplLagCritBytes {
			critLines = append(critLines, line)
		} else if lagBytes >= cfg.ReplLagWarnBytes {
			warnLines = append(warnLines, line)
		}
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G09-004", g09, "Replication lag (pg_stat_replication)", "scan error: "+err.Error())}
	}
	if !anyReplica {
		return []Finding{NewOK("G09-004", g09, "Replication lag (pg_stat_replication)",
			"No streaming replicas connected",
			"https://www.postgresql.org/docs/current/monitoring-stats.html#MONITORING-PG-STAT-REPLICATION-VIEW")}
	}
	var findings []Finding
	if len(critLines) > 0 {
		findings = append(findings, NewCrit("G09-004", g09, "Replication lag (pg_stat_replication)",
			fmt.Sprintf("%d replica(s) at critical lag", len(critLines)),
			"Investigate network or I/O bottlenecks; consider failover if lag is excessive.",
			strings.Join(critLines, "\n"),
			"https://www.postgresql.org/docs/current/monitoring-stats.html#MONITORING-PG-STAT-REPLICATION-VIEW"))
	}
	if len(warnLines) > 0 {
		findings = append(findings, NewWarn("G09-004", g09, "Replication lag (pg_stat_replication)",
			fmt.Sprintf("%d replica(s) at warning lag", len(warnLines)),
			"Monitor replication lag trends.",
			strings.Join(warnLines, "\n"),
			"https://www.postgresql.org/docs/current/monitoring-stats.html#MONITORING-PG-STAT-REPLICATION-VIEW"))
	}
	if len(findings) == 0 {
		return []Finding{NewOK("G09-004", g09, "Replication lag (pg_stat_replication)",
			"All replicas within acceptable lag thresholds",
			"https://www.postgresql.org/docs/current/monitoring-stats.html#MONITORING-PG-STAT-REPLICATION-VIEW")}
	}
	return findings
}

// G09-005 unnamed standbys
func g09UnnamedStandbys(ctx context.Context, db *pgxpool.Pool) []Finding {
	const q = `SELECT pid, client_addr
		FROM pg_stat_replication
		WHERE application_name IN ('walreceiver','') OR application_name IS NULL`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G09-005", g09, "Unnamed standbys", err.Error())}
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var pid int
		var addr *string
		_ = rows.Scan(&pid, &addr)
		ip := "(unknown)"
		if addr != nil {
			ip = *addr
		}
		lines = append(lines, fmt.Sprintf("PID %d from %s", pid, ip))
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G09-005", g09, "Unnamed standbys", "scan error: "+err.Error())}
	}
	if len(lines) == 0 {
		return []Finding{NewOK("G09-005", g09, "Unnamed standbys",
			"All standbys have meaningful application_name",
			"https://www.postgresql.org/docs/current/warm-standby.html")}
	}
	return []Finding{NewInfo("G09-005", g09, "Unnamed standbys",
		fmt.Sprintf("%d unnamed standby(ies)", len(lines)),
		"Set application_name in primary_conninfo for easier identification.",
		strings.Join(lines, "\n"),
		"https://www.postgresql.org/docs/current/warm-standby.html")}
}

// G09-006 recovery_min_apply_delay > 0
func g09RecoveryMinApplyDelay(ctx context.Context, db *pgxpool.Pool) []Finding {
	var val string
	if err := db.QueryRow(ctx, "SELECT setting FROM pg_settings WHERE name='recovery_min_apply_delay'").Scan(&val); err != nil {
		return []Finding{NewSkip("G09-006", g09, "recovery_min_apply_delay", err.Error())}
	}
	if val != "0" && val != "" {
		return []Finding{NewInfo("G09-006", g09, "recovery_min_apply_delay",
			fmt.Sprintf("recovery_min_apply_delay = %sms", val),
			"This is an intentional delayed replica configuration — document this decision.",
			"Delayed replicas provide a recovery window but increase RPO for failover scenarios.",
			"https://www.postgresql.org/docs/current/standby-settings.html")}
	}
	return []Finding{NewOK("G09-006", g09, "recovery_min_apply_delay",
		"recovery_min_apply_delay = 0 (no intentional delay)",
		"https://www.postgresql.org/docs/current/standby-settings.html")}
}

// G09-007 wal_keep_size=0 with physical standbys without slots
func g09WALKeepSize(ctx context.Context, db *pgxpool.Pool) []Finding {
	var walKeepSize int64
	_ = db.QueryRow(ctx, "SELECT setting::bigint FROM pg_settings WHERE name='wal_keep_size'").Scan(&walKeepSize)

	// Count physical standbys not using a slot
	const q = `SELECT count(*) FROM pg_stat_replication r
		WHERE r.application_name NOT IN (
			SELECT slot_name FROM pg_replication_slots WHERE slot_type='physical' AND active=true
		)
		AND r.state = 'streaming'`
	var unslottedCount int
	_ = db.QueryRow(ctx, q).Scan(&unslottedCount)

	obs := fmt.Sprintf("wal_keep_size=%dMB, unslotted streaming standbys=%d", walKeepSize, unslottedCount)
	if walKeepSize == 0 && unslottedCount > 0 {
		return []Finding{NewWarn("G09-007", g09, "wal_keep_size vs unslotted standbys", obs,
			"Set wal_keep_size >= 1024 or use replication slots for all standbys.",
			"Without wal_keep_size, standbys not using slots may lose their WAL position after a lag.",
			"https://www.postgresql.org/docs/current/runtime-config-replication.html")}
	}
	return []Finding{NewOK("G09-007", g09, "wal_keep_size vs unslotted standbys", obs,
		"https://www.postgresql.org/docs/current/runtime-config-replication.html")}
}

// G09-008 cross-reference to G02-009
func g09CrossRefG02(ctx context.Context, db *pgxpool.Pool) []Finding {
	return []Finding{NewInfo("G09-008", g09, "WAL archiving cross-reference",
		"See G02-009 for WAL .ready file backlog and G02-011 for pg_stat_archiver failures.",
		"Monitor both WAL archiving (G02) and replication slot lag (G09) together for complete WAL health.",
		"",
		"https://www.postgresql.org/docs/current/continuous-archiving.html")}
}

// ── Logical replication checks ────────────────────────────────────────────────

// G09-009  Invalidated replication slots (PG 14+)
//
// A slot becomes invalidated when PostgreSQL forcibly removes it because it
// was retaining too much WAL (max_slot_wal_keep_size exceeded) or because the
// slot fell too far behind on a standby. An invalidated slot is effectively
// dead — it can never catch up — but it continues to exist until dropped.
// It must be dropped manually: SELECT pg_drop_replication_slot('name');
func g09InvalidatedSlots(ctx context.Context, db *pgxpool.Pool) []Finding {
	// invalidation_reason column was added in PG 17
	var major int
	if err := db.QueryRow(ctx, "SELECT current_setting('server_version_num')::int / 10000").Scan(&major); err != nil {
		return []Finding{NewSkip("G09-009", g09, "Invalidated replication slots", err.Error())}
	}
	if major < 17 {
		return []Finding{NewInfo("G09-009", g09, "Invalidated replication slots",
			fmt.Sprintf("PostgreSQL %d — invalidation_reason column requires PG 17+", major),
			"Upgrade to PG 17 to gain slot invalidation reason tracking.", "",
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html")}
	}

	const q = `SELECT slot_name, slot_type, COALESCE(invalidation_reason, '') AS reason
	            FROM pg_replication_slots
	            WHERE invalidation_reason IS NOT NULL
	            ORDER BY slot_name`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G09-009", g09, "Invalidated replication slots", err.Error())}
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var name, slotType, reason string
		_ = rows.Scan(&name, &slotType, &reason)
		lines = append(lines, fmt.Sprintf("%s (%s): reason=%s", name, slotType, reason))
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G09-009", g09, "Invalidated replication slots", "scan error: "+err.Error())}
	}
	if len(lines) == 0 {
		return []Finding{NewOK("G09-009", g09, "Invalidated replication slots",
			"No invalidated slots",
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html")}
	}
	return []Finding{NewCrit("G09-009", g09, "Invalidated replication slots",
		fmt.Sprintf("%d invalidated slot(s) — must be dropped", len(lines)),
		"Drop each invalidated slot: SELECT pg_drop_replication_slot('slot_name');",
		strings.Join(lines, "\n"),
		"https://www.postgresql.org/docs/current/view-pg-replication-slots.html")}
}

// G09-010  max_slot_wal_keep_size not configured (PG 13+)
//
// The default value of -1 means slots can retain unlimited WAL. A single
// dead logical slot with no limit can silently grow to fill the entire disk.
// Setting a finite limit causes PostgreSQL to invalidate a slot rather than
// crash, which is always the safer failure mode.
func g09MaxSlotWALKeepSize(ctx context.Context, db *pgxpool.Pool) []Finding {
	var major int
	if err := db.QueryRow(ctx, "SELECT current_setting('server_version_num')::int / 10000").Scan(&major); err != nil {
		return []Finding{NewSkip("G09-010", g09, "max_slot_wal_keep_size", err.Error())}
	}
	if major < 13 {
		return []Finding{NewInfo("G09-010", g09, "max_slot_wal_keep_size",
			fmt.Sprintf("PostgreSQL %d — max_slot_wal_keep_size available in PG 13+", major),
			"", "", "https://www.postgresql.org/docs/current/runtime-config-replication.html")}
	}

	var val int64
	if err := db.QueryRow(ctx, "SELECT setting::bigint FROM pg_settings WHERE name='max_slot_wal_keep_size'").Scan(&val); err != nil {
		return []Finding{NewSkip("G09-010", g09, "max_slot_wal_keep_size", err.Error())}
	}
	obs := fmt.Sprintf("max_slot_wal_keep_size = %d MB", val)
	if val == -1 {
		return []Finding{NewWarn("G09-010", g09, "max_slot_wal_keep_size",
			"max_slot_wal_keep_size = -1 (unlimited)",
			"Set max_slot_wal_keep_size to a safe limit (e.g. 10240 = 10 GB) in postgresql.conf.",
			"With no limit a single stale slot can grow to fill the entire disk before PostgreSQL crashes.",
			"https://www.postgresql.org/docs/current/runtime-config-replication.html")}
	}
	return []Finding{NewOK("G09-010", g09, "max_slot_wal_keep_size", obs,
		"https://www.postgresql.org/docs/current/runtime-config-replication.html")}
}

// G09-011  Inactive logical replication slots
//
// Physical slot lag is captured by G09-001. This check focuses specifically on
// logical slots where active=false — these are the highest-risk slots because
// they accumulate WAL for every write on the primary, yet have no consumer
// actively reading it. Each one is a potential disk-exhaustion event.
func g09InactiveLogicalSlots(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	const q = `SELECT slot_name,
	                   COALESCE(plugin, 'unknown') AS plugin,
	                   COALESCE(database, '') AS dbname,
	                   pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn) AS lag_bytes
	            FROM pg_replication_slots
	            WHERE slot_type = 'logical' AND active = false
	            ORDER BY lag_bytes DESC NULLS LAST`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G09-011", g09, "Inactive logical slots", err.Error())}
	}
	defer rows.Close()

	var critLines, warnLines, infoLines []string
	for rows.Next() {
		var name, plugin, dbname string
		var lagBytes int64
		_ = rows.Scan(&name, &plugin, &dbname, &lagBytes)
		line := fmt.Sprintf("%s  plugin=%-18s  db=%-12s  lag=%d MB",
			name, plugin, dbname, lagBytes/1024/1024)
		switch {
		case lagBytes >= cfg.ReplLagCritBytes:
			critLines = append(critLines, line)
		case lagBytes >= cfg.ReplLagWarnBytes:
			warnLines = append(warnLines, line)
		default:
			infoLines = append(infoLines, line)
		}
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G09-011", g09, "Inactive logical slots", "scan error: "+err.Error())}
	}

	if len(critLines)+len(warnLines)+len(infoLines) == 0 {
		return []Finding{NewOK("G09-011", g09, "Inactive logical slots",
			"No inactive logical replication slots",
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html")}
	}

	var findings []Finding
	if len(critLines) > 0 {
		findings = append(findings, NewCrit("G09-011", g09, "Inactive logical slots",
			fmt.Sprintf("%d inactive logical slot(s) at critical lag", len(critLines)),
			"Drop slots that have no consumer: SELECT pg_drop_replication_slot('name');",
			strings.Join(critLines, "\n"),
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html"))
	}
	if len(warnLines) > 0 {
		findings = append(findings, NewWarn("G09-011", g09, "Inactive logical slots",
			fmt.Sprintf("%d inactive logical slot(s) at warning lag", len(warnLines)),
			"Reconnect the consumer or drop the slot if it is no longer needed.",
			strings.Join(warnLines, "\n"),
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html"))
	}
	if len(infoLines) > 0 {
		findings = append(findings, NewInfo("G09-011", g09, "Inactive logical slots",
			fmt.Sprintf("%d inactive logical slot(s) with low lag — monitor closely", len(infoLines)),
			"Confirm each slot has an active consumer scheduled to reconnect.",
			strings.Join(infoLines, "\n"),
			"https://www.postgresql.org/docs/current/view-pg-replication-slots.html"))
	}
	return findings
}

// G09-012  Logical replication worker status
//
// Each pg_subscription on a subscriber has one apply worker in pg_stat_activity.
// A subscription with no worker (pid IS NULL in pg_stat_subscription) means the
// apply process has crashed or was never started — replication has stopped silently.
// On PG 15+ we also check pg_stat_subscription_stats for accumulated errors.
func g09LogicalWorkerStatus(ctx context.Context, db *pgxpool.Pool) []Finding {
	// Check if any subscriptions exist on this node
	var subCount int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM pg_subscription").Scan(&subCount); err != nil {
		// pg_subscription not accessible or not a subscriber — skip silently
		return []Finding{NewSkip("G09-012", g09, "Logical replication worker status",
			"pg_subscription not accessible (this may not be a logical replication subscriber)")}
	}
	if subCount == 0 {
		return []Finding{NewOK("G09-012", g09, "Logical replication worker status",
			"No logical replication subscriptions on this node",
			"https://www.postgresql.org/docs/current/view-pg-stat-subscription.html")}
	}

	// Workers with no pid = apply process is not running
	const q = `SELECT subname, pid, received_lsn::text, last_msg_receipt_time
	            FROM pg_stat_subscription
	            ORDER BY subname`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G09-012", g09, "Logical replication worker status", err.Error())}
	}
	defer rows.Close()

	var dead, slow []string
	var total int
	for rows.Next() {
		total++
		var subname string
		var pid *int
		var lsn *string
		var lastReceipt *string
		_ = rows.Scan(&subname, &pid, &lsn, &lastReceipt)
		if pid == nil {
			dead = append(dead, fmt.Sprintf("%s — worker not running (pid IS NULL)", subname))
		}
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G09-012", g09, "Logical replication worker status", "scan error: "+err.Error())}
	}

	// PG 15+: check pg_stat_subscription_stats for accumulated errors
	var major int
	_ = db.QueryRow(ctx, "SELECT current_setting('server_version_num')::int / 10000").Scan(&major)
	if major >= 15 {
		const errQ = `SELECT subname, apply_error_count, sync_error_count
		              FROM pg_stat_subscription_stats
		              WHERE apply_error_count > 0 OR sync_error_count > 0`
		erows, err := db.Query(ctx, errQ)
		if err == nil {
			defer erows.Close()
			for erows.Next() {
				var subname string
				var applyErr, syncErr int64
				_ = erows.Scan(&subname, &applyErr, &syncErr)
				slow = append(slow, fmt.Sprintf("%s — apply_errors=%d  sync_errors=%d",
					subname, applyErr, syncErr))
			}
		}
	}

	if len(dead) == 0 && len(slow) == 0 {
		return []Finding{NewOK("G09-012", g09, "Logical replication worker status",
			fmt.Sprintf("%d subscription worker(s) running normally", total),
			"https://www.postgresql.org/docs/current/view-pg-stat-subscription.html")}
	}

	var findings []Finding
	if len(dead) > 0 {
		findings = append(findings, NewCrit("G09-012", g09, "Logical replication worker status",
			fmt.Sprintf("%d subscription worker(s) not running", len(dead)),
			"Check PostgreSQL logs; re-enable the subscription: ALTER SUBSCRIPTION name ENABLE;",
			strings.Join(dead, "\n"),
			"https://www.postgresql.org/docs/current/view-pg-stat-subscription.html"))
	}
	if len(slow) > 0 {
		findings = append(findings, NewWarn("G09-012", g09, "Logical replication worker status",
			fmt.Sprintf("%d subscription(s) have accumulated errors", len(slow)),
			"Check PostgreSQL logs for the root cause of apply or sync errors.",
			strings.Join(slow, "\n"),
			"https://www.postgresql.org/docs/current/view-pg-stat-subscription-stats.html"))
	}
	return findings
}

// G09-013  Subscription relation (table) sync state
//
// When a new subscription is created, PostgreSQL copies the initial data for
// each table through a sync process. Each table moves through states:
//
//	i = initialize   (sync not yet started)
//	d = data copy    (initial COPY in progress)
//	f = finished     (copy done, catching up to apply position)
//	s = synchronized (table has caught up and is being applied normally)
//	r = ready        (normal steady-state replication)
//
// Tables stuck in 'i' or 'd' indicate a stalled initial sync — either the
// sync worker crashed or was never scheduled. This means those tables are
// NOT being replicated until the sync completes.
func g09SubscriptionRelState(ctx context.Context, db *pgxpool.Pool) []Finding {
	// Check if pg_subscription_rel exists (only on subscriber nodes)
	var hasTable bool
	_ = db.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM information_schema.tables
		WHERE table_schema = 'pg_catalog' AND table_name = 'pg_subscription_rel'
	)`).Scan(&hasTable)
	if !hasTable {
		return []Finding{NewSkip("G09-013", g09, "Subscription table sync state",
			"pg_subscription_rel not found — this node is not a logical replication subscriber")}
	}

	const q = `SELECT s.subname,
	                   r.srrelid::regclass::text AS table_name,
	                   r.srsubstate
	            FROM pg_subscription_rel r
	            JOIN pg_subscription s ON s.oid = r.srsubid
	            ORDER BY s.subname, r.srrelid::regclass::text`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G09-013", g09, "Subscription table sync state", err.Error())}
	}
	defer rows.Close()

	stateLabel := map[string]string{
		"i": "initialize (not started)",
		"d": "data copy in progress",
		"f": "finished copy, catching up",
		"s": "synchronized",
		"r": "ready",
	}

	var stuck, inProgress []string
	var total int
	for rows.Next() {
		total++
		var subname, tableName, state string
		_ = rows.Scan(&subname, &tableName, &state)
		label := stateLabel[state]
		if label == "" {
			label = state
		}
		line := fmt.Sprintf("%-30s  %-40s  %s", subname, tableName, label)
		switch state {
		case "i": // not started — stalled before sync even began
			stuck = append(stuck, line)
		case "d", "f": // in progress — may be normal or may be stuck
			inProgress = append(inProgress, line)
		}
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G09-013", g09, "Subscription table sync state", "scan error: "+err.Error())}
	}

	if total == 0 {
		return []Finding{NewOK("G09-013", g09, "Subscription table sync state",
			"No subscription tables found",
			"https://www.postgresql.org/docs/current/view-pg-stat-subscription.html")}
	}
	if len(stuck) == 0 && len(inProgress) == 0 {
		return []Finding{NewOK("G09-013", g09, "Subscription table sync state",
			fmt.Sprintf("All %d subscribed table(s) in ready/synchronized state", total),
			"https://www.postgresql.org/docs/current/view-pg-stat-subscription.html")}
	}

	var findings []Finding
	if len(stuck) > 0 {
		findings = append(findings, NewCrit("G09-013", g09, "Subscription table sync state",
			fmt.Sprintf("%d table(s) stuck in 'initialize' — sync never started", len(stuck)),
			"Check PostgreSQL logs; try: ALTER SUBSCRIPTION name REFRESH PUBLICATION;",
			strings.Join(stuck, "\n"),
			"https://www.postgresql.org/docs/current/sql-altersubscription.html"))
	}
	if len(inProgress) > 0 {
		findings = append(findings, NewInfo("G09-013", g09, "Subscription table sync state",
			fmt.Sprintf("%d table(s) in initial sync (data copy or catching up)", len(inProgress)),
			"This is normal for new subscriptions — monitor until state reaches 'ready'.",
			strings.Join(inProgress, "\n"),
			"https://www.postgresql.org/docs/current/view-pg-stat-subscription.html"))
	}
	return findings
}

// G09-014 streaming replication time-based lag
// Byte-lag (G09-004) tells you how much data is outstanding.
// Time-lag (write_lag / flush_lag / replay_lag) tells you how long
// a commit had to wait. A synchronous standby with high replay_lag
// means every COMMIT on primary blocks for that duration — a hidden
// performance killer that byte-lag alone won't reveal.
func g09StreamingLagTime(ctx context.Context, db *pgxpool.Pool) []Finding {
	const q = `SELECT application_name, sync_state, state,
	            COALESCE(EXTRACT(EPOCH FROM write_lag)::int,  0) AS write_secs,
	            COALESCE(EXTRACT(EPOCH FROM flush_lag)::int,  0) AS flush_secs,
	            COALESCE(EXTRACT(EPOCH FROM replay_lag)::int, 0) AS replay_secs,
	            COALESCE(write_lag::text,  '0')  AS write_s,
	            COALESCE(flush_lag::text,  '0')  AS flush_s,
	            COALESCE(replay_lag::text, '0')  AS replay_s
	            FROM pg_stat_replication
	            ORDER BY replay_secs DESC NULLS LAST`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G09-014", g09, "Streaming replication lag (time)", err.Error())}
	}
	defer rows.Close()

	var critLines, warnLines, okLines []string
	for rows.Next() {
		var appName, syncState, state string
		var writeSecs, flushSecs, replaySecs int
		var writeS, flushS, replayS string
		_ = rows.Scan(&appName, &syncState, &state, &writeSecs, &flushSecs, &replaySecs,
			&writeS, &flushS, &replayS)
		line := fmt.Sprintf("%-22s sync=%-8s write=%-12s flush=%-12s replay=%s",
			appName, syncState, writeS, flushS, replayS)
		switch {
		case syncState == "sync" && replaySecs > 5:
			critLines = append(critLines, line)
		case replaySecs > 30:
			warnLines = append(warnLines, line)
		default:
			okLines = append(okLines, line)
		}
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G09-014", g09, "Streaming replication lag (time)", "scan error: "+err.Error())}
	}

	all := append(append(critLines, warnLines...), okLines...)
	if len(all) == 0 {
		return []Finding{NewOK("G09-014", g09, "Streaming replication lag (time)",
			"No streaming standbys connected",
			"https://www.postgresql.org/docs/current/monitoring-stats.html#MONITORING-PG-STAT-REPLICATION-VIEW")}
	}
	if len(critLines) > 0 {
		return []Finding{NewCrit("G09-014", g09, "Streaming replication lag (time)",
			fmt.Sprintf("%d synchronous standby(ies) lagging — COMMITs are blocking", len(critLines)),
			"Investigate network latency, standby I/O, or consider converting to async replication.",
			strings.Join(all, "\n"),
			"https://www.postgresql.org/docs/current/warm-standby.html#SYNCHRONOUS-REPLICATION")}
	}
	if len(warnLines) > 0 {
		return []Finding{NewWarn("G09-014", g09, "Streaming replication lag (time)",
			fmt.Sprintf("%d standby(ies) with replay_lag > 30s", len(warnLines)),
			"Check standby disk I/O and network bandwidth.",
			strings.Join(all, "\n"),
			"https://www.postgresql.org/docs/current/monitoring-stats.html#MONITORING-PG-STAT-REPLICATION-VIEW")}
	}
	return []Finding{NewOK("G09-014", g09, "Streaming replication lag (time)",
		fmt.Sprintf("%d standby(ies) — lag within acceptable range", len(all)),
		"https://www.postgresql.org/docs/current/monitoring-stats.html#MONITORING-PG-STAT-REPLICATION-VIEW")}
}

// G09-016 Orphaned replication origins
//
// pg_replication_origin stores named progress-tracking origins used by logical
// replication and Spock. In Spock environments each subscription creates an
// origin; when subscriptions are dropped and recreated the old origin is often
// left behind. These accumulate silently and can contribute to WAL retention
// by anchoring the origin LSN inside the logical decoding machinery.
//
// On Spock clusters the check matches each origin against active subscriptions
// by name. On plain PostgreSQL it reports all origins as INFO so operators can
// verify each one is intentional.
func g09OrphanedReplicationOrigins(ctx context.Context, db *pgxpool.Pool) []Finding {
	const id = "G09-016"
	const name = "Orphaned replication origins"

	const q = `
		SELECT o.roname,
		       COALESCE(s.remote_lsn::text, '') AS remote_lsn,
		       COALESCE(s.local_lsn::text,  '') AS local_lsn
		FROM   pg_replication_origin o
		LEFT   JOIN pg_replication_origin_status s ON s.local_id = o.roident
		ORDER  BY o.roname`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip(id, g09, name, err.Error())}
	}
	defer rows.Close()

	type origin struct {
		oname     string
		remoteLSN string
		localLSN  string
	}
	var origins []origin
	for rows.Next() {
		var o origin
		_ = rows.Scan(&o.oname, &o.remoteLSN, &o.localLSN)
		origins = append(origins, o)
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip(id, g09, name, "scan error: "+err.Error())}
	}

	if len(origins) == 0 {
		return []Finding{NewOK(id, g09, name,
			"No replication origins present",
			"https://www.postgresql.org/docs/current/replication-origins.html")}
	}

	// Non-Spock path: list all origins as INFO so the operator can review.
	if !spockInstalled(ctx, db) {
		var lines []string
		for _, o := range origins {
			lsn := o.remoteLSN
			if lsn == "" {
				lsn = "none"
			}
			lines = append(lines, fmt.Sprintf("%-40s  remote_lsn=%s", o.oname, lsn))
		}
		return []Finding{NewInfo(id, g09, name,
			fmt.Sprintf("%d replication origin(s) found", len(origins)),
			"Verify each origin has an active consumer; remove unused ones with pg_drop_replication_origin('name').",
			strings.Join(lines, "\n"),
			"https://www.postgresql.org/docs/current/replication-origins.html")}
	}

	// Spock path: match each origin against active subscription names.
	// Spock encodes the subscription name into the origin name, so a substring
	// match is a reliable heuristic for determining ownership.
	var subNames []string
	if spockExists(ctx, db, "subscription") {
		srows, err := db.Query(ctx, "SELECT sub_name FROM spock.subscription")
		if err == nil {
			for srows.Next() {
				var s string
				_ = srows.Scan(&s)
				subNames = append(subNames, s)
			}
			srows.Close()
		}
	}

	var orphanLines, activeLines []string
	for _, o := range origins {
		lsn := o.remoteLSN
		if lsn == "" {
			lsn = "none"
		}
		line := fmt.Sprintf("%-40s  remote_lsn=%s", o.oname, lsn)
		matched := false
		for _, sn := range subNames {
			if strings.Contains(o.oname, sn) {
				matched = true
				break
			}
		}
		if matched {
			activeLines = append(activeLines, line)
		} else {
			orphanLines = append(orphanLines, line+" ← no matching subscription")
		}
	}

	if len(orphanLines) == 0 {
		return []Finding{NewOK(id, g09, name,
			fmt.Sprintf("All %d origin(s) matched to active Spock subscriptions", len(origins)),
			"https://www.postgresql.org/docs/current/replication-origins.html")}
	}

	cleanup := "-- Drop each orphaned origin:\n"
	for _, o := range origins {
		matched := false
		for _, sn := range subNames {
			if strings.Contains(o.oname, sn) {
				matched = true
				break
			}
		}
		if !matched {
			cleanup += fmt.Sprintf("SELECT pg_drop_replication_origin('%s');\n", o.oname)
		}
	}

	detail := strings.Join(append(orphanLines, activeLines...), "\n") + "\n\n" + cleanup
	return []Finding{NewWarn(id, g09, name,
		fmt.Sprintf("%d origin(s) have no matching Spock subscription", len(orphanLines)),
		"Orphaned origins accumulate when Spock subscriptions are dropped and recreated. "+
			"Drop them with pg_drop_replication_origin() to prevent silent WAL retention and origin table bloat.",
		detail,
		"https://www.postgresql.org/docs/current/replication-origins.html")}
}

// G09-015 logical_decoding_work_mem — spill-to-disk threshold for logical slots
// Introduced in PG 13. Default 64 MB is a spill-file time bomb on busy primaries
// with multiple logical slots.
func g09LogicalDecodingWorkMem(ctx context.Context, db *pgxpool.Pool) []Finding {
	const id = "G09-015"
	const name = "logical_decoding_work_mem"

	var major int
	if err := db.QueryRow(ctx,
		"SELECT current_setting('server_version_num')::int / 10000",
	).Scan(&major); err != nil {
		return []Finding{NewSkip(id, g09, name, err.Error())}
	}
	if major < 13 {
		return []Finding{NewSkip(id, g09, name, "Requires PostgreSQL 13+")}
	}

	var workMemKB int
	if err := db.QueryRow(ctx,
		"SELECT setting::int FROM pg_settings WHERE name = 'logical_decoding_work_mem'",
	).Scan(&workMemKB); err != nil {
		return []Finding{NewSkip(id, g09, name, err.Error())}
	}

	var logicalSlots int
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pg_replication_slots WHERE slot_type = 'logical'",
	).Scan(&logicalSlots); err != nil {
		return []Finding{NewSkip(id, g09, name, err.Error())}
	}

	workMemMB := workMemKB / 1024
	obs := fmt.Sprintf("logical_decoding_work_mem = %dMB  |  logical slots = %d", workMemMB, logicalSlots)
	const defaultKB = 65536 // 64 MB

	if workMemKB <= defaultKB && logicalSlots >= 2 {
		return []Finding{NewWarn(id, g09, name, obs,
			fmt.Sprintf("Increase logical_decoding_work_mem above 64MB — %d logical slots at default will spill to disk under write load.", logicalSlots),
			"Spill files accumulate in pg_replslot/<slot>/spill/ and persist until the slot is consumed. Consider 256MB+.",
			"https://www.postgresql.org/docs/current/runtime-config-resource.html#GUC-LOGICAL-DECODING-WORK-MEM")}
	}
	if workMemKB <= defaultKB && logicalSlots >= 1 {
		return []Finding{NewInfo(id, g09, name, obs,
			"Consider increasing logical_decoding_work_mem to 256MB+ on busy primaries to avoid slot spill files.",
			"Slot decoding spills to pg_replslot/<slot>/spill/ when this limit is exceeded.",
			"https://www.postgresql.org/docs/current/runtime-config-resource.html#GUC-LOGICAL-DECODING-WORK-MEM")}
	}
	return []Finding{NewOK(id, g09, name, obs,
		"https://www.postgresql.org/docs/current/runtime-config-resource.html#GUC-LOGICAL-DECODING-WORK-MEM")}
}
