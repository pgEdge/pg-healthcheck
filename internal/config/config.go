// Package config loads healthcheck.yaml and holds all tunable thresholds.
// CLI flags override YAML values after Load() is called.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the single source of truth for every threshold used in a check.
// YAML tags match the keys in healthcheck.yaml exactly.
type Config struct {
	// ── Connection ─────────────────────────────────────────
	Host                string        `yaml:"-"` // set by CLI
	Port                int           `yaml:"-"`
	DBName              string        `yaml:"-"`
	User                string        `yaml:"-"`
	Password            string        `yaml:"-"`
	ConnectionTimeoutMS int           `yaml:"connection_timeout_ms"`
	ConnectionTimeout   time.Duration `yaml:"-"` // derived
	PgIsreadyWarnMS     int           `yaml:"pg_isready_warn_ms"`
	WarnConnectionsPct  int           `yaml:"warn_connections_pct"`
	CritConnectionsPct  int           `yaml:"critical_connections_pct"`
	IdleInTxWarnSec     int           `yaml:"idle_in_tx_warn_seconds"`
	IdleConnWarnSec     int           `yaml:"idle_conn_warn_seconds"`

	// ── TLS ────────────────────────────────────────────────
	SSLCertWarnDays int `yaml:"ssl_cert_warn_days"`
	SSLCertCritDays int `yaml:"ssl_cert_critical_days"`

	// ── pgBackRest ─────────────────────────────────────────
	BackrestConfig    string `yaml:"backrest_config"`
	BackrestStanza    string `yaml:"backrest_stanza"`
	BackupMaxAgeHours int    `yaml:"backup_max_age_hours"`
	MinRetentionFull  int    `yaml:"min_retention_full"`
	WALReadyWarnCount int    `yaml:"wal_ready_warn_count"`
	WALReadyCritCount int    `yaml:"wal_ready_critical_count"`

	// ── Queries & Locks ────────────────────────────────────
	LongQueryWarnSec int `yaml:"long_query_warn_seconds"`
	LongQueryCritSec int `yaml:"long_query_critical_seconds"`

	// ── Vacuum / TXID ──────────────────────────────────────
	TxidWrapWarnMillion int `yaml:"txid_wrap_warn_million"`
	TxidWrapCritMillion int `yaml:"txid_wrap_critical_million"`

	// ── WAL / Replication ──────────────────────────────────
	ReplLagWarnBytes int64 `yaml:"replication_lag_warn_bytes"`
	ReplLagCritBytes int64 `yaml:"replication_lag_critical_bytes"`
	WALSlotWarnGB    int   `yaml:"wal_slot_retain_warn_gb"`
	WALSlotCritGB    int   `yaml:"wal_slot_retain_critical_gb"`

	// ── Spock / pgEdge ─────────────────────────────────────
	SpockExceptionWarnRows   int `yaml:"spock_exception_log_warn_rows"`
	SpockExceptionCritRows   int `yaml:"spock_exception_log_crit_rows"`
	SpockResolutionsWarnRows int `yaml:"spock_resolutions_warn_rows"`
	SpockOldExceptionDays    int `yaml:"spock_old_exception_days"`
	SpockExceptionRetryMins  int `yaml:"spock_exception_retry_warn_mins"`
	SpillWarnMB              int `yaml:"spill_warn_mb"`
	SpillCritMB              int `yaml:"spill_critical_mb"`

	// ── Queries (G04) ──────────────────────────────────────
	SlowQueryMeanWarnMs int `yaml:"slow_query_mean_warn_ms"`

	// ── WAL Growth (G14) ───────────────────────────────────
	WALRateWarnMBs            int     `yaml:"wal_rate_warn_mb_s"`
	WALRateCritMBs            int     `yaml:"wal_rate_critical_mb_s"`
	WALDirWarnGB              int     `yaml:"wal_dir_warn_gb"`
	WALDirCritGB              int     `yaml:"wal_dir_critical_gb"`
	WALRateBaselineMultiplier float64 `yaml:"wal_rate_baseline_multiplier"`
	WALRateBaselineSamples    int     `yaml:"wal_rate_baseline_samples"`
	WALFPIRatioWarn           float64 `yaml:"wal_fpi_ratio_warn"`
	WALFilesystemWarnPct      int     `yaml:"wal_filesystem_warn_pct"`
	WALFilesystemCritPct      int     `yaml:"wal_filesystem_critical_pct"`
	WALRateStateFile          string  `yaml:"wal_rate_state_file"`

	// ── amcheck ────────────────────────────────────────────
	AmcheckTableList []string `yaml:"amcheck_table_list"`

	// ── pg_visibility ──────────────────────────────────────
	PgVisibilityTableList []string `yaml:"pg_visibility_table_list"`

	// ── Cluster mode ───────────────────────────────────────
	ClusterNodes               []string `yaml:"cluster_nodes"`
	CrossNodeTables            []string `yaml:"cross_node_tables"`
	CrossNodeCountThresholdPct float64  `yaml:"cross_node_count_threshold_pct"`

	// ── Natural Language Interface ─────────────────────────
	// LLMProvider selects the backend: "ollama" (default), "openai", "gemini".
	LLMProvider string `yaml:"llm_provider"`
	// LLMAPIKey holds the API key for cloud providers.
	// If empty, OPENAI_API_KEY or GEMINI_API_KEY env vars are checked instead.
	LLMAPIKey            string `yaml:"llm_api_key"`
	OllamaHost           string `yaml:"ollama_host"`
	OllamaModel          string `yaml:"ollama_model"`
	OllamaTimeoutSeconds int    `yaml:"ollama_timeout_seconds"`

	// ── Runtime (set by CLI, not YAML) ─────────────────────
	Mode            string        `yaml:"-"` // "single" | "cluster"
	Output          string        `yaml:"-"` // "text" | "json"
	Groups          []string      `yaml:"-"` // subset of groups; empty = all
	TargetVersion   int           `yaml:"-"` // for G10
	NoColor         bool          `yaml:"-"`
	Verbose         bool          `yaml:"-"` // show OK findings
	CheckTimeout    time.Duration `yaml:"-"` // derived
	CheckTimeoutSec int           `yaml:"check_timeout_seconds"`
}

// Defaults returns a Config pre-populated with safe production baselines.
func Defaults() *Config {
	return &Config{
		Host:                       "localhost",
		Port:                       5432,
		DBName:                     "postgres",
		User:                       "postgres",
		ConnectionTimeoutMS:        5000,
		PgIsreadyWarnMS:            500,
		WarnConnectionsPct:         75,
		CritConnectionsPct:         90,
		IdleInTxWarnSec:            30,
		IdleConnWarnSec:            3600,
		SSLCertWarnDays:            30,
		SSLCertCritDays:            7,
		BackrestConfig:             "/etc/pgbackrest/pgbackrest.conf",
		BackrestStanza:             "main",
		BackupMaxAgeHours:          26,
		MinRetentionFull:           2,
		WALReadyWarnCount:          100,
		WALReadyCritCount:          500,
		LongQueryWarnSec:           60,
		LongQueryCritSec:           300,
		SlowQueryMeanWarnMs:        5000,
		TxidWrapWarnMillion:        500,
		TxidWrapCritMillion:        200,
		ReplLagWarnBytes:           52428800,
		ReplLagCritBytes:           524288000,
		WALSlotWarnGB:              5,
		WALSlotCritGB:              20,
		SpockExceptionWarnRows:     10000,
		SpockExceptionCritRows:     100000,
		SpockResolutionsWarnRows:   50000,
		SpockOldExceptionDays:      7,
		SpockExceptionRetryMins:    60,
		SpillWarnMB:                500,
		SpillCritMB:                2000,
		WALRateWarnMBs:             50,
		WALRateCritMBs:             200,
		WALDirWarnGB:               20,
		WALDirCritGB:               50,
		WALRateBaselineMultiplier:  3.0,
		WALRateBaselineSamples:     12,
		WALFPIRatioWarn:            0.40,
		WALFilesystemWarnPct:       60,
		WALFilesystemCritPct:       80,
		WALRateStateFile:           "/tmp/pg-healthcheck_wal_rate.json",
		CrossNodeCountThresholdPct: 1.0,
		Mode:                       "single",
		Output:                     "text",
		CheckTimeoutSec:            10,
		LLMProvider:                "ollama",
		LLMAPIKey:                  "",
		OllamaHost:                 "http://localhost:11434",
		OllamaModel:                "llama3.2",
		OllamaTimeoutSeconds:       30,
	}
}

// Load reads the YAML file at path and merges values into cfg.
// Fields absent from the file keep their existing value (e.g. CLI defaults).
func Load(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parsing config %s: %w", path, err)
	}
	cfg.finalise()
	return nil
}

// finalise converts raw millisecond/second integers into time.Duration fields.
func (c *Config) finalise() {
	if c.ConnectionTimeoutMS > 0 {
		c.ConnectionTimeout = time.Duration(c.ConnectionTimeoutMS) * time.Millisecond
	} else {
		c.ConnectionTimeout = 5 * time.Second
	}
	if c.CheckTimeoutSec > 0 {
		c.CheckTimeout = time.Duration(c.CheckTimeoutSec) * time.Second
	} else {
		c.CheckTimeout = 10 * time.Second
	}
}
