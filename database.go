package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// ---- SQLite 数据库层 ----
// 使用 modernc.org/sqlite 纯 Go 驱动，无需 CGO（驱动注册名 "sqlite"）

// Account 数据库中的 ZCode 账号记录。
// user_id 为自然键：重复导入同一账号时按 user_id upsert，
// device_mid / credentials_raw 不会被后续导入"清空"（COALESCE 保留空值场景）；
// 但携带非空值的导入（OAuth 本机指纹/多实例遥测/导出包）会覆盖旧 device_mid
// （设备信号跟随导入源，有意为之）；自动生成的指纹由 CAS 保护不轮换。
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

	// 付费通道（api.z.ai API Key，按量计费）与免费通道（JWT 套餐额度）状态分离：
	// 免费侧耗尽/冷却记在 status/cooling_until，付费侧受限记在 paid_cooling_until，
	// 互不牵连——免费耗尽的账号其付费通道仍可参与回退。
	PaidFallback     bool   `json:"paid_fallback"`      // 允许付费通道参与回退（双通道账号才有意义）
	PaidCoolingUntil int64  `json:"paid_cooling_until"` // 付费通道冷却截止 epoch 秒（含余额不足长冷却）
	PaidLastError    string `json:"paid_last_error"`    // 付费侧最近错误：与免费侧 last_error 分离，
	// 否则付费 429（秒级事件）会覆盖免费侧长冷却的真实原因（风控 24h），
	// 503 提示与面板就会拿付费理由解释免费冷却时长

	// usageChannel 本次请求实际使用的通道（"free"/"paid"），转发路径在上游请求前
	// 设置、recordUsage 读取；每个请求持有独立 Account 副本，写读同 goroutine。
	usageChannel string

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
	TaskType  string `json:"task_type"` // detect | claim | activate | reset
	PlanID    string `json:"plan_id"`
	PlanName  string `json:"plan_name"`
	Success   bool   `json:"success"`
	Code      int    `json:"code"`
	Message   string `json:"message"`
	NextAt    int64  `json:"next_at"` // 1005 名额用完时的下次可领时间 epoch 毫秒
	UsedAt    int64  `json:"used_at"` // 上游重置 used_at（epoch 秒；本地执行的重置为 0）
}

// UsageRecord API 使用记录
type UsageRecord struct {
	ID                  int64  `json:"id"`
	CreatedAt           string `json:"created_at"`
	AccountID           int64  `json:"account_id"`
	Email               string `json:"email"`
	Model               string `json:"model"`
	PromptTokens        int    `json:"prompt_tokens"`
	CompletionTokens    int    `json:"completion_tokens"`
	TotalTokens         int    `json:"total_tokens"`
	CacheReadTokens     int    `json:"cache_read_tokens"`
	CacheCreationTokens int    `json:"cache_creation_tokens"`
	Stream              bool   `json:"stream"`
	StatusCode          int    `json:"status_code"`
	DurationMs          int    `json:"duration_ms"`
	TtftMs              int    `json:"ttft_ms"`
	GatewayKeyID        int64  `json:"gateway_key_id"`
	KeyName             string `json:"key_name"`
	Channel             string `json:"channel"` // free（JWT 套餐）| paid（api.z.ai 按量计费）；旧记录为空按 free
}

// ProxyNode 出口代理节点（组绑定）
type ProxyNode struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"` // socks5 | http
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	PasswordBroken bool   `json:"password_broken"` // 密文存在但当前钥匙解不开（ListProxyNodes 检测）
	IsDefault      bool   `json:"is_default"`
	GroupName      string `json:"group_name"` // 绑定的账号组（多组用逗号分隔）
	Enabled        bool   `json:"enabled"`
	CheckStatus    string `json:"check_status"`
	CheckLatency   int    `json:"check_latency"`
	CheckIP        string `json:"check_ip"`
	CheckMsg       string `json:"check_msg"`
	CheckAt        string `json:"check_at"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
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
	return NewDBWithOptions(dbPath, true)
}

// NewDBWithOptions 打开数据库。migrate=false（-doctor 体检路径）跳过一切
// 会写库的启动动作：schema DDL、加列迁移、设置播种、vault 密钥生成/轮换/
// 明文迁移（含 VACUUM）——体检一个备份库时不得顺手把它升级或换钥匙。
// 仍以只读方式解析 vault 密钥（已存在的 keyfile/env 只认领不轮换），
// 保证 doctor 的 AES 回环探测与账号读取可用；并跳过 journal_mode=WAL
// （对非 WAL 库改模式会写文件头，读路径在默认日志模式下照常工作）。
func NewDBWithOptions(dbPath string, migrate bool) (*DB, error) {
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	conn.SetMaxOpenConns(1) // SQLite 单写
	pragmas := []string{
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		// 凭证迁移/删除后，空闲页中的明文残留要在释放时即被清零
		"PRAGMA secure_delete=ON",
	}
	if migrate {
		pragmas = append([]string{"PRAGMA journal_mode=WAL"}, pragmas...)
	}
	for _, p := range pragmas {
		if _, err := conn.Exec(p); err != nil {
			conn.Close()
			return nil, fmt.Errorf("exec pragma %q: %w", p, err)
		}
	}
	db := &DB{conn: conn}
	if !migrate {
		resolveVaultSeedReadOnly(db, dbPath)
		db.ProbeVaultHealth()
		return db, nil
	}
	if err := db.initSchema(); err != nil {
		conn.Close()
		return nil, err
	}
	// 凭证加密种子解析（keyfile 生成/轮换）必须先于任何账号读写
	ResolveVaultSeed(db, dbPath)
	// priority 列增量迁移（旧库无此列）
	if err := db.addColumnMigrate(`ALTER TABLE accounts ADD COLUMN priority INTEGER NOT NULL DEFAULT 100`); err != nil {
		conn.Close()
		return nil, err
	}
	// 免费优先 / 付费回退：付费通道独立状态 + 每账号回退开关 + 用量通道归因
	for _, col := range []string{
		`ALTER TABLE accounts ADD COLUMN paid_fallback INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE accounts ADD COLUMN paid_cooling_until INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN paid_last_error TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN channel TEXT NOT NULL DEFAULT ''`,
	} {
		if err := db.addColumnMigrate(col); err != nil {
			conn.Close()
			return nil, err
		}
	}
	// usage_records 增量迁移：缓存 token 计量（R3）+ 命名网关 Key 归因（R1）
	for _, col := range []string{
		`ALTER TABLE usage_records ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN gateway_key_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN key_name TEXT NOT NULL DEFAULT ''`,
		// 上游重置 used_at（epoch 秒）：让同步去重走精确匹配而非时区换算启发式
		`ALTER TABLE claim_records ADD COLUMN used_at INTEGER NOT NULL DEFAULT 0`,
	} {
		if err := db.addColumnMigrate(col); err != nil {
			conn.Close()
			return nil, err
		}
	}
	// 存量明文凭证列静态加密迁移（幂等；失败不阻断启动，下轮再试）
	if err := db.MigrateVault(); err != nil {
		log.Printf("[vault] migrate: %v", err)
	}
	db.ProbeVaultHealth()
	return db, nil
}

// addColumnMigrate 增量加列：仅"列已存在"视为幂等成功，其余错误如实上报——
// 吞掉真失败（如库被外部进程占锁）会让启动看似成功、首个账号查询才撞
// no such column，症状离病因三步远
func (db *DB) addColumnMigrate(stmt string) error {
	if _, err := db.conn.Exec(stmt); err != nil {
		if strings.Contains(err.Error(), "duplicate column name") {
			return nil
		}
		return fmt.Errorf("migrate %q: %w", stmt, err)
	}
	log.Printf("[db] %s", stmt)
	return nil
}

// resolveVaultSeedReadOnly ResolveVaultSeed 的只读变体（-doctor 体检路径）：
// 只"认领"现有钥匙——env 显式指定，或已有 keyfile 且能解开库中全部密文。
// 绝不生成 keyfile、绝不轮换/合并重加密：体检备份库不能改变其密钥状态。
// 无法确定钥匙时停留在派生种子并告警（doctor 的 vault 探测与账号读取
// 会如实反映解不开的部分）。
func resolveVaultSeedReadOnly(db *DB, dbPath string) {
	if s := os.Getenv("ZCODE_PROXY_VAULT_SECRET"); s != "" {
		setVaultSeedOverride(s)
		return
	}
	if keyFile := vaultKeyFile(dbPath); keyFile != "" {
		if seed, ok := loadVaultKeyFile(keyFile); ok {
			_, broken, err := db.scanVaultCiphertext(seed)
			if err == nil && broken == 0 {
				setVaultSeedOverride(seed)
				return
			}
		}
	}
	legacy := legacyVaultSeed()
	total, broken, err := db.scanVaultCiphertext(legacy)
	switch {
	case err != nil:
		log.Printf("[vault] doctor: ciphertext scan failed (%v); staying on derived key", err)
	case total == 0:
		// 库中无密文（新库/无账号）：无需钥匙；真实启动才会生成 keyfile
	case broken == 0:
		log.Printf("[vault] doctor: %d plaintext-era credential value(s) would be rotated to vault.key on a real start; left untouched", total)
	default:
		log.Printf("[vault] doctor: WARNING %d of %d encrypted credential value(s) cannot be decrypted without their original key (restore data/vault.key or set ZCODE_PROXY_VAULT_SECRET)", broken, total)
	}
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
		// 自动重置策略：默认关闭；开启后仅在"耗尽 && 自然窗口等待 > 阈值"时消耗。
		// 临期消耗（use-it-or-lose-it）同为默认关：自动动用重置机会整体 opt-in，
		// 需显式开 auto_reset_expiry_enabled
		"auto_reset_enabled":              "0",
		"auto_reset_min_wait_minutes":     "60",
		"auto_reset_min_wait_week_hours":  "24",
		"auto_reset_expiry_enabled":       "0",
		"auto_reset_expiry_spend_minutes": "60",
		// 免费优先 / 付费回退：免费通道（JWT 套餐额度）先用，受限（并发满/限流/耗尽）
		// 后无缝落到付费通道（api.z.ai 按量计费）；上限 0 = 不限
		"paid_fallback_mode":   "free_first",
		"paid_daily_token_cap": "0",
	}
	for k, v := range defaults {
		if _, err := db.conn.Exec(
			`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, k, v); err != nil {
			log.Printf("[db] seed setting %s: %v", k, err)
		}
	}
	return nil
}
