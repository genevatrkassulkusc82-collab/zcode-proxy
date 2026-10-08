package main

import (
	"database/sql"
	"fmt"
	"log"
	"sync"

	_ "modernc.org/sqlite"
)

// ---- SQLite 数据库层 ----
// 使用 modernc.org/sqlite 纯 Go 驱动，无需 CGO（驱动注册名 "sqlite"）

// Account 数据库中的 ZCode 账号记录。
// user_id 为自然键：重复导入同一账号时按 user_id upsert，
// device_mid / credentials_raw 一旦写入不会被后续导入清空（COALESCE 保留）。
type Account struct {
	// mu 串行化下方运行时可变字段的写入：转发请求与额度刷新 goroutine 并发读写
	// （状态/冷却/错误/额度快照/Use-Fail 计数，见 database_accounts.go 的写入方法）。
	// Account 一律以指针传递，禁止按值复制。
	mu sync.Mutex

	ID          int64  `json:"id"`
	UserID      string `json:"user_id"`      // 自然键（JWT user_id / user_info.id）
	Email       string `json:"email"`        // 登录邮箱
	DisplayName string `json:"display_name"` // 昵称
	Provider    string `json:"provider"`     // zai | bigmodel
	AuthType    string `json:"auth_type"`    // jwt | apikey

	AccessToken  string `json:"-"`          // OAuth access_token（JWT，内含 api_key claim）
	RefreshToken string `json:"-"`          // OAuth refresh_token
	ZCodeJWT     string `json:"-"`          // Coding Plan JWT（zcode.z.ai 免费通道凭证）
	APIKey       string `json:"-"`          // api.z.ai 通道密钥（{api_key}.{secret_key}）
	UserInfo     string `json:"-"`          // 原始 user_info JSON
	DeviceMid    string `json:"device_mid"` // X-Device-Mid（设备指纹，永不被重导入覆盖）
	CredsRaw     string `json:"-"`          // 本地客户端 credentials.json 原始内容（供一键切回）

	Status       string  `json:"status"`      // active|exhausted|cooling|invalid|disabled|inactive
	Enabled      bool    `json:"enabled"`     // 是否参与轮询
	AccountGroup string  `json:"group"`       // 分组（空=未分组）
	Priority     int64   `json:"priority"`    // priority 策略：数值小者优先（50=促销/免费层默认，100=普通默认）
	QuotaJSON    string  `json:"-"`           // 最近一次额度快照（规范化 JSON）
	PlanTier     string  `json:"plan_tier"`   // Start Plan / Lite / Pro / Max / 体验
	PlanExpire   string  `json:"plan_expire"` // 套餐到期时间（展示用字符串）
	TotalUnits   float64 `json:"total_units"`
	UsedUnits    float64 `json:"used_units"`
	Remaining    float64 `json:"remaining"`

	UseCount      int    `json:"use_count"`
	FailCount     int    `json:"fail_count"`
	LastUsedAt    int64  `json:"last_used_at"`    // epoch 秒
	LastCheckedAt int64  `json:"last_checked_at"` // 额度刷新时间 epoch 秒
	CoolingUntil  int64  `json:"cooling_until"`   // 冷却截止 epoch 秒
	LastError     string `json:"last_error"`

	LastClaimAt   string `json:"last_claim_at"`   // 最近活动领取时间
	LastClaimPlan string `json:"last_claim_plan"` // 最近领取的活动名
	LastClaimMsg  string `json:"last_claim_msg"`  // 最近领取结果

	Remark    string `json:"remark"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ClaimPlan 活动计划（cron 调度）：检测活动 / 一键领取 / 激活套餐
type ClaimPlan struct {
	ID            int64  `json:"id"`
	PlanName      string `json:"plan_name"`
	CronExpr      string `json:"cron_expr"` // 5 段: 分 时 日 月 周
	IsActive      bool   `json:"is_active"`
	TargetType    string `json:"target_type"`   // all_accounts | single_account | group
	AccountID     int64  `json:"account_id"`    // single_account 时有效
	AccountGroup  string `json:"account_group"` // group 时有效
	TaskType      string `json:"task_type"`     // detect | claim | activate
	AutoPick      bool   `json:"auto_pick"`     // claim 时自动选优先级最高的活动
	DelaySeconds  int    `json:"delay_seconds"` // 多账号间隔秒数（防风控）
	LastRunAt     string `json:"last_run_at"`
	LastRunStatus string `json:"last_run_status"`
	LastRunMsg    string `json:"last_run_msg"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// ClaimRecord 活动领取记录
type ClaimRecord struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
	AccountID int64  `json:"account_id"`
	Email     string `json:"email"`
	TaskType  string `json:"task_type"` // detect | claim | activate
	PlanID    string `json:"plan_id"`
	PlanName  string `json:"plan_name"`
	Success   bool   `json:"success"`
	Code      int    `json:"code"`
	Message   string `json:"message"`
	NextAt    int64  `json:"next_at"` // 1005 名额用完时的下次可领时间 epoch 毫秒
}

// UsageRecord API 使用记录
type UsageRecord struct {
	ID                 int64  `json:"id"`
	CreatedAt          string `json:"created_at"`
	AccountID          int64  `json:"account_id"`
	Email              string `json:"email"`
	Model              string `json:"model"`
	PromptTokens       int    `json:"prompt_tokens"`
	CompletionTokens   int    `json:"completion_tokens"`
	TotalTokens        int    `json:"total_tokens"`
	CacheReadTokens    int    `json:"cache_read_tokens"`
	CacheCreationTokens int   `json:"cache_creation_tokens"`
	Stream             bool   `json:"stream"`
	StatusCode         int    `json:"status_code"`
	DurationMs         int    `json:"duration_ms"`
	TtftMs             int    `json:"ttft_ms"`
	GatewayKeyID       int64  `json:"gateway_key_id"`
	KeyName            string `json:"key_name"`
}

// ProxyNode 出口代理节点（组绑定）
type ProxyNode struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"` // socks5 | http
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	IsDefault    bool   `json:"is_default"`
	GroupName    string `json:"group_name"` // 绑定的账号组（多组用逗号分隔）
	Enabled      bool   `json:"enabled"`
	CheckStatus  string `json:"check_status"`
	CheckLatency int    `json:"check_latency"`
	CheckIP      string `json:"check_ip"`
	CheckMsg     string `json:"check_msg"`
	CheckAt      string `json:"check_at"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// PlanRunRecord 计划运行记录
type PlanRunRecord struct {
	ID           int64  `json:"id"`
	PlanID       int64  `json:"plan_id"`
	PlanName     string `json:"plan_name"`
	TaskType     string `json:"task_type"`
	TargetType   string `json:"target_type"`
	AccountID    int64  `json:"account_id"`
	RunAt        string `json:"run_at"`
	Status       string `json:"status"`
	Message      string `json:"message"`
	Total        int    `json:"total"`
	SuccessCount int    `json:"success_count"`
	FailCount    int    `json:"fail_count"`
	DurationMs   int    `json:"duration_ms"`
}

// DB 持有数据库连接
type DB struct {
	conn *sql.DB

	// 设置项读缓存（TTL 见 database_settings.go）：转发热路径每请求读
	// fingerprint/sticky/strategy 等多个设置，逐条 SQLite 查询是纯开销。
	// 仅进程内缓存；外部直改 sqlite 最迟 3s 生效。
	setMu    sync.Mutex
	setCache map[string]settingsCacheEntry
}

// NewDB 打开/创建 SQLite 数据库并初始化 schema
func NewDB(dbPath string) (*DB, error) {
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	conn.SetMaxOpenConns(1) // SQLite 单写
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		// 凭证迁移/删除后，空闲页中的明文残留要在释放时即被清零
		"PRAGMA secure_delete=ON",
	}
	for _, p := range pragmas {
		if _, err := conn.Exec(p); err != nil {
			conn.Close()
			return nil, fmt.Errorf("exec pragma %q: %w", p, err)
		}
	}
	db := &DB{conn: conn}
	if err := db.initSchema(); err != nil {
		conn.Close()
		return nil, err
	}
	// 凭证加密种子解析（keyfile 生成/轮换）必须先于任何账号读写
	ResolveVaultSeed(db, dbPath)
	// priority 列增量迁移（旧库无此列；已存在时报错忽略）
	if _, err := db.conn.Exec(`ALTER TABLE accounts ADD COLUMN priority INTEGER NOT NULL DEFAULT 100`); err == nil {
		log.Printf("[db] added accounts.priority column (default 100)")
	}
	// usage_records 增量迁移：缓存 token 计量（R3）+ 命名网关 Key 归因（R1）
	for _, col := range []string{
		`ALTER TABLE usage_records ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN gateway_key_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN key_name TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.conn.Exec(col); err == nil {
			log.Printf("[db] %s", col)
		}
	}
	// 存量明文凭证列静态加密迁移（幂等；失败不阻断启动，下轮再试）
	if err := db.MigrateVault(); err != nil {
		log.Printf("[vault] migrate: %v", err)
	}
	db.ProbeVaultHealth()
	return db, nil
}

// Close 关闭数据库连接
func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS accounts (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id          TEXT NOT NULL UNIQUE,
		email            TEXT DEFAULT '',
		display_name     TEXT DEFAULT '',
		provider         TEXT DEFAULT 'zai',
		auth_type        TEXT DEFAULT 'jwt',
		access_token     TEXT DEFAULT '',
		refresh_token    TEXT DEFAULT '',
		zcode_jwt        TEXT DEFAULT '',
		api_key          TEXT DEFAULT '',
		user_info        TEXT DEFAULT '',
		device_mid       TEXT DEFAULT '',
		creds_raw        TEXT DEFAULT '',
		status           TEXT DEFAULT 'active',
		enabled          INTEGER DEFAULT 1,
		account_group    TEXT DEFAULT '',
		quota_json       TEXT DEFAULT '',
		plan_tier        TEXT DEFAULT '',
		plan_expire      TEXT DEFAULT '',
		total_units      REAL DEFAULT 0,
		used_units       REAL DEFAULT 0,
		remaining        REAL DEFAULT 0,
		use_count        INTEGER DEFAULT 0,
		fail_count       INTEGER DEFAULT 0,
		last_used_at     INTEGER DEFAULT 0,
		last_checked_at  INTEGER DEFAULT 0,
		cooling_until    INTEGER DEFAULT 0,
		last_error       TEXT DEFAULT '',
		last_claim_at    TEXT DEFAULT '',
		last_claim_plan  TEXT DEFAULT '',
		last_claim_msg   TEXT DEFAULT '',
		remark           TEXT DEFAULT '',
		created_at       TEXT DEFAULT (datetime('now','localtime')),
		updated_at       TEXT DEFAULT (datetime('now','localtime'))
	);
	CREATE INDEX IF NOT EXISTS idx_accounts_enabled ON accounts(enabled);
	CREATE INDEX IF NOT EXISTS idx_accounts_status  ON accounts(status);
	CREATE INDEX IF NOT EXISTS idx_accounts_group   ON accounts(account_group);

	CREATE TABLE IF NOT EXISTS settings (
		key        TEXT PRIMARY KEY,
		value      TEXT DEFAULT '',
		updated_at TEXT DEFAULT (datetime('now','localtime'))
	);

	CREATE TABLE IF NOT EXISTS claim_plans (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		plan_name       TEXT DEFAULT '',
		cron_expr       TEXT NOT NULL DEFAULT '0 9 * * *',
		is_active       INTEGER DEFAULT 1,
		target_type     TEXT DEFAULT 'all_accounts',
		account_id      INTEGER DEFAULT 0,
		account_group   TEXT DEFAULT '',
		task_type       TEXT DEFAULT 'claim',
		auto_pick       INTEGER DEFAULT 1,
		delay_seconds   INTEGER DEFAULT 30,
		last_run_at     TEXT DEFAULT '',
		last_run_status TEXT DEFAULT '',
		last_run_msg    TEXT DEFAULT '',
		created_at      TEXT DEFAULT (datetime('now','localtime')),
		updated_at      TEXT DEFAULT (datetime('now','localtime'))
	);
	CREATE INDEX IF NOT EXISTS idx_claim_plans_active ON claim_plans(is_active);

	CREATE TABLE IF NOT EXISTS claim_records (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at  TEXT DEFAULT (datetime('now','localtime')),
		account_id  INTEGER DEFAULT 0,
		email       TEXT DEFAULT '',
		task_type   TEXT DEFAULT 'claim',
		plan_id     TEXT DEFAULT '',
		plan_name   TEXT DEFAULT '',
		success     INTEGER DEFAULT 0,
		code        INTEGER DEFAULT 0,
		message     TEXT DEFAULT '',
		next_at     INTEGER DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_claim_records_account ON claim_records(account_id);
	CREATE INDEX IF NOT EXISTS idx_claim_records_created ON claim_records(created_at);

	CREATE TABLE IF NOT EXISTS usage_records (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at        TEXT DEFAULT (datetime('now','localtime')),
		account_id        INTEGER DEFAULT 0,
		email             TEXT DEFAULT '',
		model             TEXT DEFAULT '',
		prompt_tokens     INTEGER DEFAULT 0,
		completion_tokens INTEGER DEFAULT 0,
		total_tokens      INTEGER DEFAULT 0,
		stream            INTEGER DEFAULT 0,
		status_code       INTEGER DEFAULT 0,
		duration_ms       INTEGER DEFAULT 0,
		ttft_ms           INTEGER DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_usage_records_created ON usage_records(created_at);
	CREATE INDEX IF NOT EXISTS idx_usage_records_account ON usage_records(account_id);
	CREATE INDEX IF NOT EXISTS idx_usage_records_model   ON usage_records(model);

	CREATE TABLE IF NOT EXISTS proxy_nodes (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		name          TEXT DEFAULT '',
		type          TEXT DEFAULT 'socks5',
		host          TEXT DEFAULT '',
		port          INTEGER DEFAULT 0,
		username      TEXT DEFAULT '',
		password      TEXT DEFAULT '',
		is_default    INTEGER DEFAULT 0,
		group_name    TEXT DEFAULT '',
		enabled       INTEGER DEFAULT 1,
		check_status  TEXT DEFAULT '',
		check_latency INTEGER DEFAULT 0,
		check_ip      TEXT DEFAULT '',
		check_msg     TEXT DEFAULT '',
		check_at      TEXT DEFAULT '',
		created_at    TEXT DEFAULT (datetime('now','localtime')),
		updated_at    TEXT DEFAULT (datetime('now','localtime'))
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_proxy_nodes_default ON proxy_nodes(is_default) WHERE is_default = 1;

	CREATE TABLE IF NOT EXISTS plan_run_records (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		plan_id       INTEGER DEFAULT 0,
		plan_name     TEXT DEFAULT '',
		task_type     TEXT DEFAULT '',
		target_type   TEXT DEFAULT '',
		account_id    INTEGER DEFAULT 0,
		run_at        TEXT DEFAULT (datetime('now','localtime')),
		status        TEXT DEFAULT '',
		message       TEXT DEFAULT '',
		total         INTEGER DEFAULT 0,
		success_count INTEGER DEFAULT 0,
		fail_count    INTEGER DEFAULT 0,
		duration_ms   INTEGER DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_plan_run_records_run_at ON plan_run_records(run_at);
	CREATE INDEX IF NOT EXISTS idx_plan_run_records_plan   ON plan_run_records(plan_id);

	CREATE TABLE IF NOT EXISTS gateway_keys (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		name        TEXT DEFAULT '',
		key_hash    TEXT NOT NULL UNIQUE,
		key_prefix  TEXT DEFAULT '',
		enabled     INTEGER DEFAULT 1,
		rpm_limit   INTEGER DEFAULT 0,
		quota_total INTEGER DEFAULT 0,
		quota_used  INTEGER DEFAULT 0,
		models      TEXT DEFAULT '',
		last_used_at INTEGER DEFAULT 0,
		created_at  TEXT DEFAULT (datetime('now','localtime')),
		updated_at  TEXT DEFAULT (datetime('now','localtime'))
	);
	`
	if _, err := db.conn.Exec(schema); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	// 默认设置项。注意 is_default_password 不在此播种：该标记只由认证引导
	// 显式写入（随机口令生成时置 1，修改口令时清 0），无标记 = 非缺省口令
	defaults := map[string]string{
		"admin_user":             "admin",
		"api_key":                "",
		"selection_strategy":     "round_robin",
		"quota_refresh_interval": "60",
		"app_version":            "",
		"upstream_proxy":         "",
		"fingerprint":            "chrome",
		"custom_ja3":             "",
		"captcha_mode":           "auto",
		"gateway_models":         "",
		// R5 prompt-cache 断点默认关闭（上游各通道对 cache_control 支持未全量实测）
		"prompt_cache_breakpoint": "0",
		// 速度：验证参数后台保温，转发零求解等待
		"captcha_prewarm": "1",
		// 自动领取促销活动（只领活动，绝不自动消耗重置）
		"auto_claim_promos":           "1",
		"auto_claim_interval_minutes": "30",
		"auto_claim_delay_seconds":    "10",
		// 自动重置策略：默认关闭；开启后仅在"耗尽 && 自然窗口等待 > 阈值"时消耗
		"auto_reset_enabled":           "0",
		"auto_reset_min_wait_minutes":  "60",
		"auto_reset_min_wait_week_hours": "24",
	}
	for k, v := range defaults {
		if _, err := db.conn.Exec(
			`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, k, v); err != nil {
			log.Printf("[db] seed setting %s: %v", k, err)
		}
	}
	return nil
}
