package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ---- 账号 CRUD ----

// UpsertAccount 按 user_id 自然键插入或更新账号。
// device_mid / creds_raw 用 COALESCE 保留旧值：
// 新值为空时不会被后续导入清空（设备指纹与本地凭证快照不被抹掉）。
func (db *DB) UpsertAccount(a *Account) (int64, error) {
	// 凭证列静态加密（写入密文，内存结构保持明文供调用方继续使用）；
	// 任一列加密失败即中止写入——不得静默落明文
	encAccess, err := vaultEncrypt(a.AccessToken)
	if err != nil {
		return 0, err
	}
	encRefresh, err := vaultEncrypt(a.RefreshToken)
	if err != nil {
		return 0, err
	}
	encJWT, err := vaultEncrypt(a.ZCodeJWT)
	if err != nil {
		return 0, err
	}
	encAPIKey, err := vaultEncrypt(a.APIKey)
	if err != nil {
		return 0, err
	}
	encUserInfo, err := vaultEncrypt(a.UserInfo)
	if err != nil {
		return 0, err
	}
	encCredsRaw, err := vaultEncrypt(a.CredsRaw)
	if err != nil {
		return 0, err
	}
	if _, err := db.conn.Exec(`
		INSERT INTO accounts (
			user_id, email, display_name, provider, auth_type,
			access_token, refresh_token, zcode_jwt, api_key, user_info,
			device_mid, creds_raw, status, enabled, account_group, priority, remark
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET
			email         = COALESCE(NULLIF(excluded.email,''), accounts.email),
			display_name  = COALESCE(NULLIF(excluded.display_name,''), accounts.display_name),
			provider      = excluded.provider,
			auth_type     = excluded.auth_type,
			access_token  = COALESCE(NULLIF(excluded.access_token,''), accounts.access_token),
			refresh_token = COALESCE(NULLIF(excluded.refresh_token,''), accounts.refresh_token),
			zcode_jwt     = COALESCE(NULLIF(excluded.zcode_jwt,''), accounts.zcode_jwt),
			api_key       = COALESCE(NULLIF(excluded.api_key,''), accounts.api_key),
			user_info     = COALESCE(NULLIF(excluded.user_info,''), accounts.user_info),
			device_mid    = COALESCE(NULLIF(excluded.device_mid,''), accounts.device_mid),
			creds_raw     = COALESCE(NULLIF(excluded.creds_raw,''), accounts.creds_raw),
			status        = CASE WHEN accounts.status IN ('disabled') THEN accounts.status ELSE excluded.status END,
			enabled       = excluded.enabled,
			account_group = COALESCE(NULLIF(excluded.account_group,''), accounts.account_group),
			priority      = accounts.priority,
			remark        = COALESCE(NULLIF(excluded.remark,''), accounts.remark),
			updated_at    = datetime('now','localtime')`,
		a.UserID, a.Email, a.DisplayName, a.Provider, a.AuthType,
		encAccess, encRefresh, encJWT, encAPIKey, encUserInfo,
		a.DeviceMid, encCredsRaw, a.Status, boolInt(a.Enabled), a.AccountGroup, accountPriority(a), a.Remark); err != nil {
		return 0, err
	}
	// 冲突更新分支不会推进 last_insert_rowid，驱动返回的是连接上一次
	// INSERT 的残留值——用它会给错误账户写额度/状态。按自然键回查真实 ID。
	var id int64
	row := db.conn.QueryRow(`SELECT id FROM accounts WHERE user_id = ?`, a.UserID)
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// DefaultPriority / PromoPriority priority 策略默认值：数值小者优先被选
const (
	DefaultPriority = int64(100)
	PromoPriority   = int64(50)
)

// accountPriority 归一化：未设置/非法值回落默认
func accountPriority(a *Account) int64 {
	if a.Priority > 0 {
		return a.Priority
	}
	return DefaultPriority
}

const accountCols = `id, user_id, email, display_name, provider, auth_type,
	access_token, refresh_token, zcode_jwt, api_key, user_info, device_mid, creds_raw,
	status, enabled, account_group, priority, quota_json, plan_tier, plan_expire,
	total_units, used_units, remaining, use_count, fail_count,
	last_used_at, last_checked_at, cooling_until, last_error,
	last_claim_at, last_claim_plan, last_claim_msg, remark, created_at, updated_at,
	paid_fallback, paid_cooling_until`

func scanAccount(row interface{ Scan(...interface{}) error }) (*Account, error) {
	var a Account
	var enabled int
	var paidFallback int
	err := row.Scan(
		&a.ID, &a.UserID, &a.Email, &a.DisplayName, &a.Provider, &a.AuthType,
		&a.AccessToken, &a.RefreshToken, &a.ZCodeJWT, &a.APIKey, &a.UserInfo, &a.DeviceMid, &a.CredsRaw,
		&a.Status, &enabled, &a.AccountGroup, &a.Priority, &a.QuotaJSON, &a.PlanTier, &a.PlanExpire,
		&a.TotalUnits, &a.UsedUnits, &a.Remaining, &a.UseCount, &a.FailCount,
		&a.LastUsedAt, &a.LastCheckedAt, &a.CoolingUntil, &a.LastError,
		&a.LastClaimAt, &a.LastClaimPlan, &a.LastClaimMsg, &a.Remark, &a.CreatedAt, &a.UpdatedAt,
		&paidFallback, &a.PaidCoolingUntil)
	if err != nil {
		return nil, err
	}
	a.Enabled = enabled == 1
	a.PaidFallback = paidFallback == 1
	// 凭证列静态加密：读取时透明解密
	a.AccessToken = vaultDecrypt(a.AccessToken)
	a.RefreshToken = vaultDecrypt(a.RefreshToken)
	a.ZCodeJWT = vaultDecrypt(a.ZCodeJWT)
	a.APIKey = vaultDecrypt(a.APIKey)
	a.UserInfo = vaultDecrypt(a.UserInfo)
	a.CredsRaw = vaultDecrypt(a.CredsRaw)
	return &a, nil
}

// ---- 运行时字段并发保护 ----
// 转发请求 goroutine 与额度刷新 goroutine 会并发读写 Account 的状态/额度字段，
// 所有内存写入必须经下列方法（内部持 a.mu）执行；DB 落库由调用方跟进。

// setRuntime 写状态 / 最近错误 / 冷却截止
func (a *Account) setRuntime(status, lastError string, coolingUntil int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Status = status
	a.LastError = lastError
	a.CoolingUntil = coolingUntil
}

// bumpUse 记录一次成功使用；仅 cooling 恢复 active（exhausted 只能由额度刷新恢复）
func (a *Account) bumpUse() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UseCount++
	a.LastUsedAt = time.Now().Unix()
	if a.Status == StatusCooling {
		a.Status = StatusActive
		a.CoolingUntil = 0
	}
}

// bumpFail 记录一次失败
func (a *Account) bumpFail(lastError string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.FailCount++
	a.LastError = lastError
}

// tryRecoverActive 额度刷新成功后将可恢复状态迁移回 active
// （exhausted / inactive / invalid / 已到期的 cooling）；返回是否发生迁移
func (a *Account) tryRecoverActive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.Status == StatusExhausted, a.Status == StatusInactive, a.Status == StatusInvalid:
	case a.Status == StatusCooling && (a.CoolingUntil <= 0 || time.Now().Unix() >= a.CoolingUntil):
	default:
		return false
	}
	a.Status = StatusActive
	a.CoolingUntil = 0
	a.LastError = ""
	return true
}

// setQuota 写额度快照字段
func (a *Account) setQuota(quotaJSON, planTier, planExpire string, total, used, remaining float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.QuotaJSON = quotaJSON
	a.PlanTier = planTier
	a.PlanExpire = planExpire
	a.TotalUnits = total
	a.UsedUnits = used
	a.Remaining = remaining
	a.LastCheckedAt = time.Now().Unix()
}

// lastCheckedAt 读最近额度检查时间（锁保护；转发侧刷新节流判断用）
func (a *Account) lastCheckedAt() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.LastCheckedAt
}

// statusError 读状态与最近错误（锁保护；转发侧失败提示用）
func (a *Account) statusError() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Status, a.LastError
}

// setUsageChannel 标记本次请求实际使用的通道（"free"/"paid"）；recordUsage 读取。
// 每个请求持有独立 Account 副本，写读同 goroutine，mu 仅防刷新 goroutine 并发读。
func (a *Account) setUsageChannel(ch string) {
	a.mu.Lock()
	a.usageChannel = ch
	a.mu.Unlock()
}

// usageChannelName 读通道归因；空（旧路径/闲时队列未标记）按免费通道计
func (a *Account) usageChannelName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.usageChannel == "" {
		return "free"
	}
	return a.usageChannel
}

// paidCoolingActive 付费通道冷却是否生效中（锁保护）
func (a *Account) paidCoolingActive(now int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return now < a.PaidCoolingUntil
}

// setPaidRuntime 写付费通道冷却（内存副本；DB 由调用方跟进）
func (a *Account) setPaidRuntime(lastError string, coolingUntil int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LastError = lastError
	a.PaidCoolingUntil = coolingUntil
}

// bumpUsePaid 记录一次付费通道成功使用；只清付费冷却，不动免费侧 status/cooling
// （免费侧冷却只能由免费通道成功或自然到期解除）
func (a *Account) bumpUsePaid() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UseCount++
	a.LastUsedAt = time.Now().Unix()
	a.PaidCoolingUntil = 0
}

// credentialSnapshot 锁保护地读取凭证三元组（供独立 goroutine 如异步 settle 使用，
// 避免与 setCredentials 并发读写）
func (a *Account) credentialSnapshot() (jwt, apiKey, deviceMid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ZCodeJWT, a.APIKey, a.DeviceMid
}

// setCredentials 刷新成功后就地更新内存凭证（调用方持有该实例的独占使用权：
// 每个请求/刷新 goroutine 的 Account 都是 ListAccounts 的独立副本）。
// 不同步内存的话，同一次刷新流程后续的上游调用（如重置历史同步）仍会
// 拿旧 JWT 打 401。
func (a *Account) setCredentials(accessToken, refreshToken, zcodeJWT string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.AccessToken = accessToken
	a.RefreshToken = refreshToken
	a.ZCodeJWT = zcodeJWT
}

// GetAccount 按 ID 查询
func (db *DB) GetAccount(id int64) (*Account, error) {
	row := db.conn.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("账号不存在: %d", id)
	}
	return a, err
}

// GetAccountByUserID 按自然键查询
func (db *DB) GetAccountByUserID(userID string) (*Account, error) {
	row := db.conn.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE user_id = ?`, userID)
	a, err := scanAccount(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// ListAccounts 列出账号；group 非空时按组过滤
func (db *DB) ListAccounts(group string) ([]*Account, error) {
	query := `SELECT ` + accountCols + ` FROM accounts`
	var args []interface{}
	if group != "" {
		query += ` WHERE account_group = ?`
		args = append(args, group)
	}
	query += ` ORDER BY created_at ASC`
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAccount 删除账号
func (db *DB) DeleteAccount(id int64) error {
	_, err := db.conn.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	return err
}

// UpdateAccountFields 更新账号可变字段（UI 编辑：备注/分组/启用/付费回退开关）
func (db *DB) UpdateAccountFields(id int64, group, remark string, enabled, paidFallback bool) error {
	_, err := db.conn.Exec(`
		UPDATE accounts SET account_group = ?, remark = ?, enabled = ?, paid_fallback = ?,
		status = CASE WHEN ? = 0 AND status != 'disabled' THEN 'disabled'
		             WHEN ? = 1 AND status = 'disabled' THEN 'active'
		             ELSE status END,
		updated_at = datetime('now','localtime') WHERE id = ?`,
		group, remark, boolInt(enabled), boolInt(paidFallback), boolInt(enabled), boolInt(enabled), id)
	return err
}

// SetAccountPaidStatus 写付费通道冷却（含余额不足长冷却）；不触碰免费侧状态列
func (db *DB) SetAccountPaidStatus(id int64, lastError string, coolingUntil int64) error {
	_, err := db.conn.Exec(`
		UPDATE accounts SET last_error = ?, paid_cooling_until = ?,
		updated_at = datetime('now','localtime') WHERE id = ?`,
		lastError, coolingUntil, id)
	return err
}

// TouchAccountPaidUse 付费通道成功使用（只清付费冷却；免费侧 status 不动）
func (db *DB) TouchAccountPaidUse(id int64) error {
	_, err := db.conn.Exec(`
		UPDATE accounts SET use_count = use_count + 1, last_used_at = ?,
		paid_cooling_until = 0,
		updated_at = datetime('now','localtime') WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// UpdateAccountTokens 更新凭证字段（OAuth 刷新 / 手动编辑）；凭证列静态加密
// UpdateAccountPriority 手动调整 priority 策略权重（0 回落默认 100）
func (db *DB) UpdateAccountPriority(id int64, priority int64) error {
	if priority <= 0 {
		priority = DefaultPriority
	}
	_, err := db.conn.Exec(`UPDATE accounts SET priority = ?,
		updated_at = datetime('now','localtime') WHERE id = ?`, priority, id)
	return err
}

func (db *DB) UpdateAccountTokens(id int64, accessToken, refreshToken, zcodeJWT, apiKey, userInfo string) error {
	encAccess, err := vaultEncrypt(accessToken)
	if err != nil {
		return err
	}
	encRefresh, err := vaultEncrypt(refreshToken)
	if err != nil {
		return err
	}
	encJWT, err := vaultEncrypt(zcodeJWT)
	if err != nil {
		return err
	}
	encAPIKey, err := vaultEncrypt(apiKey)
	if err != nil {
		return err
	}
	encUserInfo, err := vaultEncrypt(userInfo)
	if err != nil {
		return err
	}
	_, err = db.conn.Exec(`
		UPDATE accounts SET
			access_token  = COALESCE(NULLIF(?,''), access_token),
			refresh_token = COALESCE(NULLIF(?,''), refresh_token),
			zcode_jwt     = COALESCE(NULLIF(?,''), zcode_jwt),
			api_key       = COALESCE(NULLIF(?,''), api_key),
			user_info     = COALESCE(NULLIF(?,''), user_info),
			updated_at = datetime('now','localtime')
		WHERE id = ?`,
		encAccess, encRefresh, encJWT, encAPIKey, encUserInfo, id)
	return err
}

// SetAccountStatus 设置账号状态（含冷却时间）
func (db *DB) SetAccountStatus(id int64, status, lastError string, coolingUntil int64) error {
	_, err := db.conn.Exec(`
		UPDATE accounts SET status = ?, last_error = ?, cooling_until = ?,
		updated_at = datetime('now','localtime') WHERE id = ?`,
		status, lastError, coolingUntil, id)
	return err
}

// SetAccountQuota 写入额度快照
func (db *DB) SetAccountQuota(id int64, quotaJSON, planTier, planExpire string, total, used, remaining float64) error {
	_, err := db.conn.Exec(`
		UPDATE accounts SET quota_json = ?, plan_tier = ?, plan_expire = ?,
		total_units = ?, used_units = ?, remaining = ?,
		last_checked_at = ?, updated_at = datetime('now','localtime') WHERE id = ?`,
		quotaJSON, planTier, planExpire, total, used, remaining, time.Now().Unix(), id)
	return err
}

// TouchAccountUse 记录一次成功使用（仅 cooling 恢复 active；exhausted 等额度刷新恢复）
func (db *DB) TouchAccountUse(id int64) error {
	_, err := db.conn.Exec(`
		UPDATE accounts SET use_count = use_count + 1, last_used_at = ?,
		status = CASE WHEN status = 'cooling' THEN 'active' ELSE status END,
		cooling_until = 0,
		updated_at = datetime('now','localtime') WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// BumpAccountFail 记录一次失败
func (db *DB) BumpAccountFail(id int64, lastError string) error {
	_, err := db.conn.Exec(`
		UPDATE accounts SET fail_count = fail_count + 1, last_error = ?,
		updated_at = datetime('now','localtime') WHERE id = ?`, lastError, id)
	return err
}

// SetAccountClaimResult 记录活动领取结果
func (db *DB) SetAccountClaimResult(id int64, planName, msg string) error {
	_, err := db.conn.Exec(`
		UPDATE accounts SET last_claim_at = datetime('now','localtime'),
		last_claim_plan = ?, last_claim_msg = ?,
		updated_at = datetime('now','localtime') WHERE id = ?`, planName, msg, id)
	return err
}

// ListGroups 返回所有已用分组名（账号 + 代理节点合并去重）
func (db *DB) ListGroups() ([]string, error) {
	rows, err := db.conn.Query(`
		SELECT account_group FROM accounts WHERE account_group != ''
		UNION
		SELECT group_name FROM proxy_nodes WHERE group_name != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			continue
		}
		// group_name 可能是逗号分隔多组
		for _, part := range strings.Split(g, ",") {
			part = strings.TrimSpace(part)
			if part != "" && !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	return out, rows.Err()
}

// CountAccountsByStatus 状态统计（仪表盘）
func (db *DB) CountAccountsByStatus() (map[string]int, error) {
	rows, err := db.conn.Query(`SELECT status, COUNT(*) FROM accounts GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err == nil {
			out[s] = n
		}
	}
	return out, rows.Err()
}
