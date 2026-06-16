package checks

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgedge/pg-healthcheck/internal/config"
)

const g01 = "Connection & Availability"

var pgEOL = map[int]time.Time{
	9:  time.Date(2022, 11, 10, 0, 0, 0, 0, time.UTC),
	10: time.Date(2022, 11, 10, 0, 0, 0, 0, time.UTC),
	11: time.Date(2023, 11, 9, 0, 0, 0, 0, time.UTC),
	12: time.Date(2024, 11, 14, 0, 0, 0, 0, time.UTC),
	13: time.Date(2025, 11, 13, 0, 0, 0, 0, time.UTC),
	14: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC),
	15: time.Date(2027, 11, 11, 0, 0, 0, 0, time.UTC),
	16: time.Date(2028, 11, 9, 0, 0, 0, 0, time.UTC),
	17: time.Date(2029, 11, 8, 0, 0, 0, 0, time.UTC),
}

// G01Connection checks connection availability and related settings.
type G01Connection struct{}

func (g *G01Connection) Name() string    { return g01 }
func (g *G01Connection) GroupID() string { return "G01" }

func (g *G01Connection) Run(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) ([]Finding, error) {
	var f []Finding
	f = append(f, g01TCPReach(cfg)...)
	f = append(f, g01RTT(ctx, db, cfg)...)
	f = append(f, g01SSL(ctx, db)...)
	f = append(f, g01TLSExpiry(ctx, db, cfg)...)
	f = append(f, g01VersionEOL(ctx, db)...)
	f = append(f, g01ConnSaturation(ctx, db, cfg)...)
	f = append(f, g01IdleInTx(ctx, db, cfg)...)
	f = append(f, g01PerDBConns(ctx, db, cfg)...)
	f = append(f, g01HBATrust(ctx, db)...)
	f = append(f, g01LongIdleConn(ctx, db, cfg)...)
	return f, nil
}

// G01-001 TCP reachability
func g01TCPReach(cfg *config.Config) []Finding {
	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	t0 := time.Now()
	conn, err := net.DialTimeout("tcp", addr, cfg.ConnectionTimeout)
	ms := time.Since(t0).Milliseconds()
	if err != nil {
		return []Finding{NewCrit("G01-001", g01, "TCP port reachability",
			fmt.Sprintf("Cannot connect to %s: %v", addr, err),
			"Verify PostgreSQL is running and the port is reachable.", "",
			"https://www.postgresql.org/docs/current/server-start.html")}
	}
	_ = conn.Close()
	return []Finding{NewOK("G01-001", g01, "TCP port reachability",
		fmt.Sprintf("Connected to %s in %dms", addr, ms),
		"https://www.postgresql.org/docs/current/server-start.html")}
}

// G01-002 pg_isready RTT
func g01RTT(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	t0 := time.Now()
	var n int
	_ = db.QueryRow(ctx, "SELECT 1").Scan(&n)
	ms := time.Since(t0).Milliseconds()
	obs := fmt.Sprintf("RTT %dms", ms)
	if ms > int64(cfg.PgIsreadyWarnMS) {
		return []Finding{NewWarn("G01-002", g01, "pg_isready response time", obs,
			fmt.Sprintf("Should be < %dms", cfg.PgIsreadyWarnMS), "",
			"https://www.postgresql.org/docs/current/app-pg-isready.html")}
	}
	return []Finding{NewOK("G01-002", g01, "pg_isready response time", obs,
		"https://www.postgresql.org/docs/current/app-pg-isready.html")}
}

// G01-003 SSL enabled
func g01SSL(ctx context.Context, db *pgxpool.Pool) []Finding {
	var s string
	if err := db.QueryRow(ctx, "SELECT setting FROM pg_settings WHERE name='ssl'").Scan(&s); err != nil {
		return []Finding{NewSkip("G01-003", g01, "SSL/TLS enabled", err.Error())}
	}
	if s != "on" {
		return []Finding{NewWarn("G01-003", g01, "SSL/TLS enabled", "ssl = off",
			"Add ssl=on + ssl_cert_file/ssl_key_file to postgresql.conf.",
			"Plaintext connections expose credentials and data.",
			"https://www.postgresql.org/docs/current/ssl-tcp.html")}
	}
	return []Finding{NewOK("G01-003", g01, "SSL/TLS enabled", "ssl = on",
		"https://www.postgresql.org/docs/current/ssl-tcp.html")}
}

// G01-004 TLS certificate expiry
func g01TLSExpiry(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	// Skip cert check when ssl=off — a TLS dial would return EOF and mislead the operator.
	var sslSetting string
	if err := db.QueryRow(ctx, "SELECT setting FROM pg_settings WHERE name='ssl'").Scan(&sslSetting); err == nil && sslSetting != "on" {
		return []Finding{NewSkip("G01-004", g01, "TLS certificate expiry",
			"ssl=off — enable ssl in postgresql.conf to check certificate expiry")}
	}
	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: cfg.ConnectionTimeout},
		"tcp", addr,
		&tls.Config{InsecureSkipVerify: true}, //#nosec G402
	)
	if err != nil {
		return []Finding{NewInfo("G01-004", g01, "TLS certificate expiry",
			fmt.Sprintf("Cannot retrieve TLS cert: %v", err),
			"Enable ssl=on and provide a valid certificate.", "",
			"https://www.postgresql.org/docs/current/ssl-tcp.html")}
	}
	defer func() { _ = conn.Close() }()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return []Finding{NewInfo("G01-004", g01, "TLS certificate expiry",
			"No certificate returned", "", "",
			"https://www.postgresql.org/docs/current/ssl-tcp.html")}
	}
	exp := certs[0].NotAfter
	days := int(time.Until(exp).Hours() / 24)
	obs := fmt.Sprintf("Expires %s (%d days)", exp.Format("2006-01-02"), days)
	if days <= cfg.SSLCertCritDays {
		return []Finding{NewCrit("G01-004", g01, "TLS certificate expiry", obs,
			"Renew the certificate immediately.",
			fmt.Sprintf("%d days left — below critical threshold (%d days).", days, cfg.SSLCertCritDays),
			"https://www.postgresql.org/docs/current/ssl-tcp.html")}
	}
	if days <= cfg.SSLCertWarnDays {
		return []Finding{NewWarn("G01-004", g01, "TLS certificate expiry", obs,
			fmt.Sprintf("Renew within %d days.", days), "",
			"https://www.postgresql.org/docs/current/ssl-tcp.html")}
	}
	return []Finding{NewOK("G01-004", g01, "TLS certificate expiry", obs,
		"https://www.postgresql.org/docs/current/ssl-tcp.html")}
}

// G01-005 Version EOL
func g01VersionEOL(ctx context.Context, db *pgxpool.Pool) []Finding {
	var ver string
	if err := db.QueryRow(ctx, "SELECT current_setting('server_version')").Scan(&ver); err != nil {
		return []Finding{NewSkip("G01-005", g01, "PostgreSQL version EOL", err.Error())}
	}
	var major int
	_, _ = fmt.Sscanf(ver, "%d.", &major)
	obs := fmt.Sprintf("PostgreSQL %s (major %d)", ver, major)
	eol, known := pgEOL[major]
	if !known {
		return []Finding{NewInfo("G01-005", g01, "PostgreSQL version EOL", obs, "",
			"Verify EOL at https://www.postgresql.org/support/versioning/",
			"https://www.postgresql.org/support/versioning/")}
	}
	if time.Now().After(eol) {
		return []Finding{NewCrit("G01-005", g01, "PostgreSQL version EOL",
			fmt.Sprintf("%s — EOL was %s", obs, eol.Format("2006-01-02")),
			"Upgrade to a supported version immediately.",
			"No security fixes are provided after EOL.",
			"https://www.postgresql.org/support/versioning/")}
	}
	daysLeft := int(time.Until(eol).Hours() / 24)
	if daysLeft < 180 {
		return []Finding{NewInfo("G01-005", g01, "PostgreSQL version EOL",
			fmt.Sprintf("%s — EOL in %d days (%s)", obs, daysLeft, eol.Format("2006-01-02")),
			"Plan upgrade before EOL date.", "",
			"https://www.postgresql.org/support/versioning/")}
	}
	return []Finding{NewOK("G01-005", g01, "PostgreSQL version EOL",
		fmt.Sprintf("%s — supported until %s", obs, eol.Format("2006-01-02")),
		"https://www.postgresql.org/support/versioning/")}
}

// G01-006 Connection saturation
// Counts only client backend connections — excludes autovacuum workers, walsender,
// background workers, and other internal processes that do not consume user-facing slots.
func g01ConnSaturation(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	const q = `SELECT count(*),
		(SELECT setting::int FROM pg_settings WHERE name='max_connections'),
		(SELECT setting::int FROM pg_settings WHERE name='superuser_reserved_connections')
		FROM pg_stat_activity
		WHERE backend_type = 'client backend'`
	var active, maxConn, reserved int
	if err := db.QueryRow(ctx, q).Scan(&active, &maxConn, &reserved); err != nil {
		return []Finding{NewSkip("G01-006", g01, "Connection saturation", err.Error())}
	}
	usable := maxConn - reserved
	if usable <= 0 {
		usable = 1
	}
	pct := active * 100 / usable
	obs := fmt.Sprintf("%d/%d connections (%d%%)", active, usable, pct)
	if pct >= cfg.CritConnectionsPct {
		return []Finding{NewCrit("G01-006", g01, "Connection saturation", obs,
			"Deploy PgBouncer or reduce connection count immediately.",
			"Connection exhaustion causes all new connections to fail.",
			"https://www.postgresql.org/docs/current/runtime-config-connection.html")}
	}
	if pct >= cfg.WarnConnectionsPct {
		return []Finding{NewWarn("G01-006", g01, "Connection saturation", obs,
			fmt.Sprintf("Keep below %d%% — consider PgBouncer.", cfg.WarnConnectionsPct), "",
			"https://www.postgresql.org/docs/current/runtime-config-connection.html")}
	}
	return []Finding{NewOK("G01-006", g01, "Connection saturation", obs,
		"https://www.postgresql.org/docs/current/runtime-config-connection.html")}
}

// G01-007 Idle-in-transaction connections
func g01IdleInTx(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	const q = `SELECT pid, usename, datname,
		EXTRACT(EPOCH FROM (now() - state_change))::int AS age
		FROM pg_stat_activity
		WHERE state = 'idle in transaction'
		AND state_change < now() - ($1 * interval '1 second')
		ORDER BY age DESC LIMIT 10`
	rows, err := db.Query(ctx, q, cfg.IdleInTxWarnSec)
	if err != nil {
		return []Finding{NewSkip("G01-007", g01, "Idle-in-transaction connections", err.Error())}
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var pid, age int
		var user, dbn string
		_ = rows.Scan(&pid, &user, &dbn, &age)
		found = append(found, fmt.Sprintf("PID %d (%ds) %s@%s", pid, age, user, dbn))
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G01-007", g01, "Idle-in-transaction connections", "scan error: "+err.Error())}
	}
	if len(found) == 0 {
		return []Finding{NewOK("G01-007", g01, "Idle-in-transaction connections",
			fmt.Sprintf("None older than %ds", cfg.IdleInTxWarnSec),
			"https://www.postgresql.org/docs/current/runtime-config-client.html")}
	}
	return []Finding{NewWarn("G01-007", g01, "Idle-in-transaction connections",
		fmt.Sprintf("%d session(s) idle in transaction > %ds", len(found), cfg.IdleInTxWarnSec),
		"Set idle_in_transaction_session_timeout='30s'.",
		strings.Join(found, "\n"),
		"https://www.postgresql.org/docs/current/runtime-config-client.html")}
}

// G01-008 Per-database connection counts
func g01PerDBConns(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	var maxConn int
	_ = db.QueryRow(ctx, "SELECT setting::int FROM pg_settings WHERE name='max_connections'").Scan(&maxConn)
	if maxConn <= 0 {
		maxConn = 100
	}
	const q = `SELECT datname, count(*) FROM pg_stat_activity
		WHERE datname IS NOT NULL GROUP BY datname ORDER BY 2 DESC`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G01-008", g01, "Per-database connection counts", err.Error())}
	}
	defer rows.Close()
	var findings []Finding
	for rows.Next() {
		var dbn string
		var cnt int
		_ = rows.Scan(&dbn, &cnt)
		if cnt*100/maxConn > 50 {
			findings = append(findings, NewInfo("G01-008", g01, "Per-database connection counts",
				fmt.Sprintf("%s: %d conns (%d%% of max)", dbn, cnt, cnt*100/maxConn),
				"Consider a dedicated connection pool for this database.", "",
				"https://www.postgresql.org/docs/current/monitoring-stats.html"))
		}
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G01-008", g01, "Per-database connection counts", "scan error: "+err.Error())}
	}
	if len(findings) == 0 {
		return []Finding{NewOK("G01-008", g01, "Per-database connection counts",
			"No database exceeds 50% of max_connections",
			"https://www.postgresql.org/docs/current/monitoring-stats.html")}
	}
	return findings
}

// G01-010 Long-lived idle connections
// Distinct from G01-007 (idle in transaction): these connections hold no open
// transaction but still consume a backend slot and may retain an older snapshot,
// preventing horizon advancement. Common symptom of a leaking application pool.
func g01LongIdleConn(ctx context.Context, db *pgxpool.Pool, cfg *config.Config) []Finding {
	const q = `SELECT pid, usename, datname, application_name,
		COALESCE(client_addr::text, 'local') AS client_addr,
		EXTRACT(EPOCH FROM (now() - state_change))::int AS idle_secs
		FROM pg_stat_activity
		WHERE state = 'idle'
		  AND backend_type = 'client backend'
		  AND state_change < now() - ($1 * interval '1 second')
		ORDER BY idle_secs DESC
		LIMIT 10`
	rows, err := db.Query(ctx, q, cfg.IdleConnWarnSec)
	if err != nil {
		return []Finding{NewSkip("G01-010", g01, "Long-lived idle connections", err.Error())}
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var pid, idleSecs int
		var user, dbn, appName, addr string
		_ = rows.Scan(&pid, &user, &dbn, &appName, &addr, &idleSecs)
		found = append(found, fmt.Sprintf("PID %d (%s) %s@%s app=%s idle=%s",
			pid, addr, user, dbn, appName, g14FmtSecs(int64(idleSecs))))
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G01-010", g01, "Long-lived idle connections", "scan error: "+err.Error())}
	}
	if len(found) == 0 {
		return []Finding{NewOK("G01-010", g01, "Long-lived idle connections",
			fmt.Sprintf("No idle connections older than %s", g14FmtSecs(int64(cfg.IdleConnWarnSec))),
			"https://www.postgresql.org/docs/current/runtime-config-client.html")}
	}
	return []Finding{NewWarn("G01-010", g01, "Long-lived idle connections",
		fmt.Sprintf("%d idle connection(s) older than %s", len(found), g14FmtSecs(int64(cfg.IdleConnWarnSec))),
		"Review application connection pool idle-timeout settings; terminate stale connections with pg_terminate_backend(pid).",
		strings.Join(found, "\n"),
		"https://www.postgresql.org/docs/current/runtime-config-client.html")}
}

// G01-009 pg_hba TRUST on non-loopback
func g01HBATrust(ctx context.Context, db *pgxpool.Pool) []Finding {
	const q = `SELECT type, array_to_string(database, ','), array_to_string(user_name, ','), address
		FROM pg_hba_file_rules
		WHERE auth_method = 'trust'
		AND type IN ('host','hostssl','hostnossl')
		AND address NOT IN ('127.0.0.1','::1')
		AND address NOT LIKE '127.%'`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return []Finding{NewSkip("G01-009", g01, "pg_hba trust on non-loopback", err.Error())}
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var typ, dbs, users, addr string
		_ = rows.Scan(&typ, &dbs, &users, &addr)
		found = append(found, fmt.Sprintf("%s db=%s user=%s addr=%s", typ, dbs, users, addr))
	}
	if err := rows.Err(); err != nil {
		return []Finding{NewSkip("G01-009", g01, "pg_hba trust on non-loopback", "scan error: "+err.Error())}
	}
	if len(found) == 0 {
		return []Finding{NewOK("G01-009", g01, "pg_hba trust on non-loopback",
			"No TRUST entries on non-loopback addresses",
			"https://www.postgresql.org/docs/current/auth-pg-hba-conf.html")}
	}
	return []Finding{NewCrit("G01-009", g01, "pg_hba trust on non-loopback",
		fmt.Sprintf("%d TRUST rule(s) on non-loopback", len(found)),
		"Replace TRUST with scram-sha-256 in pg_hba.conf immediately.",
		strings.Join(found, "\n"),
		"https://www.postgresql.org/docs/current/auth-pg-hba-conf.html")}
}
