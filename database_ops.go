package main

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---- 活动计划 CRUD ----

func (db *DB) ListClaimPlans() ([]*ClaimPlan, error) {
	rows, err := db.conn.Query(`
		SELECT id, plan_name, cron_expr, is_active, target_type, account_id, account_group,
		       task_type, auto_pick, delay_seconds, last_run_at, last_run_status, last_run_msg,
		       created_at, updated_at
		FROM claim_plans ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ClaimPlan
	for rows.Next() {
		var p ClaimPlan
		var active, autoPick int
		if err := rows.Scan(&p.ID, &p.PlanName, &p.CronExpr, &active, &p.TargetType,
			&p.AccountID, &p.AccountGroup, &p.TaskType, &autoPick, &p.DelaySeconds,
			&p.LastRunAt, &p.LastRunStatus, &p.LastRunMsg, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.IsActive = active == 1
		p.AutoPick = autoPick == 1
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (db *DB) GetClaimPlan(id int64) (*ClaimPlan, error) {
	plans, err := db.ListClaimPlans()
	if err != nil {
		return nil, err
	}
	for _, p := range plans {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (db *DB) SaveClaimPlan(p *ClaimPlan) (int64, error) {
	if p.ID > 0 {
		_, err := db.conn.Exec(`
			UPDATE claim_plans SET plan_name=?, cron_expr=?, is_active=?, target_type=?,
			account_id=?, account_group=?, task_type=?, auto_pick=?, delay_seconds=?,
			updated_at=datetime('now','localtime') WHERE id=?`,
			p.PlanName, p.CronExpr, boolInt(p.IsActive), p.TargetType,
			p.AccountID, p.AccountGroup, p.TaskType, boolInt(p.AutoPick), p.DelaySeconds, p.ID)
		return p.ID, err
	}
	res, err := db.conn.Exec(`
		INSERT INTO claim_plans (plan_name, cron_expr, is_active, target_type,
			account_id, account_group, task_type, auto_pick, delay_seconds)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		p.PlanName, p.CronExpr, boolInt(p.IsActive), p.TargetType,
		p.AccountID, p.AccountGroup, p.TaskType, boolInt(p.AutoPick), p.DelaySeconds)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) DeleteClaimPlan(id int64) error {
	_, err := db.conn.Exec(`DELETE FROM claim_plans WHERE id = ?`, id)
	return err
}

// UpdateClaimPlanRunAt 写计划运行状态，显式记录触发时间：
// 执行完成时间 ≠ 触发分钟，按完成时间写会让每分钟计划在分钟边界处漏跑
func (db *DB) UpdateClaimPlanRunAt(id int64, status, msg, runAt string) error {
	_, err := db.conn.Exec(`
		UPDATE claim_plans SET last_run_at=?,
		last_run_status=?, last_run_msg=? WHERE id=?`, runAt, status, msg, id)
	return err
}

// ---- 活动领取记录 ----

// HasResetRecordNear 是否已存在该账号 ±15 分钟内、同一重置类型（kind）的成功记录
// （用于上游 used_at 去重：官方客户端等外部执行的重置不必重复入库。
// 必须按 kind 区分：five_hour 与 week 背靠背消耗时互不构成重复）
// 注意：created_at 存的是 localtime 墙钟字符串，strftime('%s') 会按 UTC 解析，
// 需减去本地时区偏移才是真实 epoch。
func (db *DB) HasResetRecordNear(accountID int64, usedAtSec int64, kind string) (bool, error) {
	_, offset := time.Now().Zone()
	var n int
	err := db.conn.QueryRow(
		`SELECT COUNT(1) FROM claim_records
		 WHERE account_id=? AND task_type='reset' AND success=1
		   AND plan_name LIKE ?
		   AND (used_at = ? OR (used_at = 0 AND ABS(strftime('%s',created_at)-?-?)<900))`,
		accountID, "%("+kind+")%", usedAtSec, offset, usedAtSec).Scan(&n)
	return n > 0, err
}

func (db *DB) InsertClaimRecord(r *ClaimRecord) error {
	_, err := db.conn.Exec(`
		INSERT INTO claim_records (account_id, email, task_type, plan_id, plan_name, success, code, message, next_at, used_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.AccountID, r.Email, r.TaskType, r.PlanID, r.PlanName,
		boolInt(r.Success), r.Code, r.Message, r.NextAt, r.UsedAt)
	return err
}

func (db *DB) ListClaimRecords(limit int, accountID int64) ([]*ClaimRecord, error) {
	query := `SELECT id, created_at, account_id, email, task_type, plan_id, plan_name, success, code, message, next_at, used_at
		FROM claim_records`
	var args []interface{}
	if accountID > 0 {
		query += ` WHERE account_id = ?`
		args = append(args, accountID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ClaimRecord
	for rows.Next() {
		var r ClaimRecord
		var success int
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.AccountID, &r.Email, &r.TaskType,
			&r.PlanID, &r.PlanName, &success, &r.Code, &r.Message, &r.NextAt, &r.UsedAt); err != nil {
			return nil, err
		}
		r.Success = success == 1
		out = append(out, &r)
	}
	return out, rows.Err()
}

// ---- 使用记录 ----

func (db *DB) InsertUsageRecord(r *UsageRecord) error {
	_, err := db.conn.Exec(`
		INSERT INTO usage_records (account_id, email, model, prompt_tokens, completion_tokens,
			total_tokens, cache_read_tokens, cache_creation_tokens, stream, status_code,
			duration_ms, ttft_ms, gateway_key_id, key_name, channel)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.AccountID, r.Email, r.Model, r.PromptTokens, r.CompletionTokens,
		r.TotalTokens, r.CacheReadTokens, r.CacheCreationTokens, boolInt(r.Stream),
		r.StatusCode, r.DurationMs, r.TtftMs, r.GatewayKeyID, r.KeyName, r.Channel)
	return err
}

// PaidTokensToday 当日（本地时区）付费通道 token 消耗合计，paid_daily_token_cap 限额判断用
func (db *DB) PaidTokensToday() (int64, error) {
	var n int64
	err := db.conn.QueryRow(`
		SELECT COALESCE(SUM(total_tokens), 0) FROM usage_records
		WHERE channel = 'paid'
		  AND created_at >= datetime('now','localtime','start of day')`).Scan(&n)
	return n, err
}

func (db *DB) ListUsageRecords(limit int) ([]*UsageRecord, error) {
	rows, err := db.conn.Query(`
		SELECT id, created_at, account_id, email, model, prompt_tokens, completion_tokens,
		       total_tokens, cache_read_tokens, cache_creation_tokens, stream, status_code,
		       duration_ms, ttft_ms, gateway_key_id, key_name, channel
		FROM usage_records ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UsageRecord
	for rows.Next() {
		var r UsageRecord
		var stream int
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.AccountID, &r.Email, &r.Model,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens,
			&r.CacheReadTokens, &r.CacheCreationTokens, &stream,
			&r.StatusCode, &r.DurationMs, &r.TtftMs, &r.GatewayKeyID, &r.KeyName, &r.Channel); err != nil {
			return nil, err
		}
		r.Stream = stream == 1
		out = append(out, &r)
	}
	return out, rows.Err()
}

// UsageStats 聚合统计（报表页）：总量 / 缓存命中 / 成功率 / TTFT 分位 /
// 按模型 / 按账号 / 按下游 Key / 按天趋势
func (db *DB) UsageStats(days int) (map[string]interface{}, error) {
	since := time.Now().AddDate(0, 0, -days).Format("2006-01-02 15:04:05")
	out := map[string]interface{}{}
	row := db.conn.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
		       COALESCE(SUM(total_tokens),0), COALESCE(SUM(cache_read_tokens),0),
		       COALESCE(SUM(cache_creation_tokens),0),
		       COALESCE(AVG(duration_ms),0), COALESCE(AVG(NULLIF(ttft_ms,0)),0),
		       COALESCE(SUM(CASE WHEN status_code < 400 THEN 1 ELSE 0 END),0)
		FROM usage_records WHERE created_at >= ?`, since)
	var n, pt, ct, tt, crt, cct, ok int
	var avgDur, avgTtft float64
	if err := row.Scan(&n, &pt, &ct, &tt, &crt, &cct, &avgDur, &avgTtft, &ok); err != nil {
		return nil, err
	}
	out["requests"] = n
	out["prompt_tokens"] = pt
	out["completion_tokens"] = ct
	out["total_tokens"] = tt
	out["cache_read_tokens"] = crt
	out["cache_creation_tokens"] = cct
	out["avg_duration_ms"] = int(avgDur)
	out["avg_ttft_ms"] = int(avgTtft)
	if n > 0 {
		out["success_rate"] = float64(ok) / float64(n)
		// 缓存命中率 = 命中 token /（命中 + 新建 + 未缓存输入）
		if denom := crt + cct + pt; denom > 0 {
			out["cache_hit_rate"] = float64(crt) / float64(denom)
		}
	} else {
		out["success_rate"] = 0.0
		out["cache_hit_rate"] = 0.0
	}
	// TTFT 分位（非零样本，内存计算；7d 个人量级足够）
	if ttfts, err := db.ttftSamples(since, 20000); err == nil && len(ttfts) > 0 {
		out["p50_ttft_ms"] = percentile(ttfts, 0.5)
		out["p95_ttft_ms"] = percentile(ttfts, 0.95)
	}

	// 按模型分布
	models := map[string]int{}
	rows, err := db.conn.Query(`SELECT model, COUNT(*) FROM usage_records WHERE created_at >= ? GROUP BY model`, since)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var m string
			var c int
			if rows.Scan(&m, &c) == nil {
				models[m] = c
			}
		}
	}
	out["by_model"] = models

	// 按账号分布
	accounts := map[string]int{}
	rows2, err := db.conn.Query(`SELECT MAX(email), COUNT(*) FROM usage_records WHERE created_at >= ? GROUP BY account_id`, since)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var e string
			var c int
			if rows2.Scan(&e, &c) == nil {
				if e == "" {
					e = "unknown"
				}
				accounts[e] = c
			}
		}
	}
	out["by_account"] = accounts

	// 按下游网关 Key 分布（R1）：请求数 + token 消耗（配额页展示用）
	type keyAgg struct {
		Requests int    `json:"requests"`
		Tokens   int64  `json:"tokens"`
		Name     string `json:"name"`
	}
	byKey := map[string]*keyAgg{}
	rows3, err := db.conn.Query(`
		SELECT COALESCE(NULLIF(key_name,''),'(root)'), gateway_key_id, COUNT(*), COALESCE(SUM(total_tokens),0)
		FROM usage_records WHERE created_at >= ? GROUP BY gateway_key_id`, since)
	if err == nil {
		defer rows3.Close()
		for rows3.Next() {
			var name string
			var kid, c int64
			var tok int64
			if rows3.Scan(&name, &kid, &c, &tok) == nil {
				key := strconv.FormatInt(kid, 10)
				agg, exists := byKey[key]
				if !exists {
					agg = &keyAgg{Name: name}
					byKey[key] = agg
				}
				agg.Requests += int(c)
				agg.Tokens += tok
			}
		}
	}
	out["by_gateway_key"] = byKey

	// 按天趋势：请求数 / token / 缓存命中（前端图表）
	type dayTrend struct {
		Day       string `json:"day"`
		Requests  int    `json:"requests"`
		Tokens    int64  `json:"tokens"`
		CacheRead int64  `json:"cache_read_tokens"`
	}
	var trend []dayTrend
	rows4, err := db.conn.Query(`
		SELECT date(created_at), COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cache_read_tokens),0)
		FROM usage_records WHERE created_at >= ? GROUP BY date(created_at) ORDER BY date(created_at)`, since)
	if err == nil {
		defer rows4.Close()
		for rows4.Next() {
			var d dayTrend
			var c int
			if rows4.Scan(&d.Day, &c, &d.Tokens, &d.CacheRead) == nil {
				d.Requests = c
				trend = append(trend, d)
			}
		}
	}
	out["daily"] = trend
	return out, nil
}

// ttftSamples 窗口内非零 TTFT 样本（升序返回，供分位计算）
func (db *DB) ttftSamples(since string, cap int) ([]int, error) {
	rows, err := db.conn.Query(`
		SELECT ttft_ms FROM usage_records
		WHERE created_at >= ? AND ttft_ms > 0 ORDER BY ttft_ms LIMIT ?`, since, cap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if rows.Scan(&v) == nil {
			out = append(out, v)
		}
	}
	return out, rows.Err()
}

// percentile 有序样本的最近秩分位
func percentile(sorted []int, q float64) int {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(q * float64(len(sorted)-1))
	return sorted[idx]
}

// ---- 代理节点 CRUD ----

func (db *DB) ListProxyNodes() ([]*ProxyNode, error) {
	rows, err := db.conn.Query(`
		SELECT id, name, type, host, port, username, password, is_default, group_name, enabled,
		       check_status, check_latency, check_ip, check_msg, check_at, created_at, updated_at
		FROM proxy_nodes ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProxyNode
	for rows.Next() {
		var n ProxyNode
		var isDef, enabled int
		if err := rows.Scan(&n.ID, &n.Name, &n.Type, &n.Host, &n.Port, &n.Username, &n.Password,
			&isDef, &n.GroupName, &enabled, &n.CheckStatus, &n.CheckLatency, &n.CheckIP,
			&n.CheckMsg, &n.CheckAt, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		// 密码静态加密读回明文（vault1: 前缀才解；历史明文行原样透传，
		// 下次保存时加密迁移）
		n.Password = vaultDecrypt(n.Password)
		n.IsDefault = isDef == 1
		n.Enabled = enabled == 1
		out = append(out, &n)
	}
	return out, rows.Err()
}

func (db *DB) SaveProxyNode(n *ProxyNode) (int64, error) {
	// 密码与账号凭证同一威胁模型：库文件外泄不得连带可用代理凭证
	enc, err := vaultEncrypt(n.Password)
	if err != nil {
		return 0, fmt.Errorf("proxy password encrypt: %w", err)
	}
	n.Password = enc
	// 清默认 + 写新默认必须同事务：清了不写会留下零默认节点，
	// 写了不清会撞 idx_proxy_nodes_default 唯一约束
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if n.IsDefault {
		if _, err := tx.Exec(`UPDATE proxy_nodes SET is_default = 0`); err != nil {
			return 0, err
		}
	}
	if n.ID > 0 {
		// 空密码 = 保持原值（面板"留空不改"语义）：不触碰密码列——经解密
		// 读回再回写，会在钥匙缺失期把解不开的密文冲成 '' 永久销毁
		if n.Password == "" {
			_, err := tx.Exec(`
				UPDATE proxy_nodes SET name=?, type=?, host=?, port=?, username=?,
				is_default=?, group_name=?, enabled=?, updated_at=datetime('now','localtime') WHERE id=?`,
				n.Name, n.Type, n.Host, n.Port, n.Username,
				boolInt(n.IsDefault), n.GroupName, boolInt(n.Enabled), n.ID)
			if err != nil {
				return 0, err
			}
			return n.ID, tx.Commit()
		}
		_, err := tx.Exec(`
			UPDATE proxy_nodes SET name=?, type=?, host=?, port=?, username=?, password=?,
			is_default=?, group_name=?, enabled=?, updated_at=datetime('now','localtime') WHERE id=?`,
			n.Name, n.Type, n.Host, n.Port, n.Username, n.Password,
			boolInt(n.IsDefault), n.GroupName, boolInt(n.Enabled), n.ID)
		if err != nil {
			return 0, err
		}
		return n.ID, tx.Commit()
	}
	res, err := tx.Exec(`
		INSERT INTO proxy_nodes (name, type, host, port, username, password, is_default, group_name, enabled)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		n.Name, n.Type, n.Host, n.Port, n.Username, n.Password,
		boolInt(n.IsDefault), n.GroupName, boolInt(n.Enabled))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) DeleteProxyNode(id int64) error {
	_, err := db.conn.Exec(`DELETE FROM proxy_nodes WHERE id = ?`, id)
	return err
}

func (db *DB) UpdateProxyNodeCheck(id int64, status string, latency int, ip, msg string) error {
	_, err := db.conn.Exec(`
		UPDATE proxy_nodes SET check_status=?, check_latency=?, check_ip=?, check_msg=?,
		check_at=datetime('now','localtime') WHERE id=?`, status, latency, ip, msg, id)
	return err
}

// ProxyNodeForGroup 查找组绑定的启用代理节点；组无绑定则回退默认节点。
// group_name 支持逗号分隔多组。
func (db *DB) ProxyNodeForGroup(group string) (*ProxyNode, error) {
	nodes, err := db.ListProxyNodes()
	if err != nil {
		return nil, err
	}
	if group != "" {
		for _, n := range nodes {
			if !n.Enabled {
				continue
			}
			for _, g := range strings.Split(n.GroupName, ",") {
				if strings.TrimSpace(g) == group {
					return n, nil
				}
			}
		}
	}
	for _, n := range nodes {
		if n.Enabled && n.IsDefault {
			return n, nil
		}
	}
	return nil, nil
}

// ---- 计划运行记录 ----

func (db *DB) InsertPlanRunRecord(r *PlanRunRecord) error {
	if r.RunAt == "" {
		// 触发分钟 ≠ 完成时间：调用方没带时回填空串不如取计划行上的
		// last_run_at——但最起码不用完成时刻冒充触发时刻
		r.RunAt = time.Now().Format("2006-01-02 15:04:05")
	}
	_, err := db.conn.Exec(`
		INSERT INTO plan_run_records (plan_id, plan_name, task_type, target_type, account_id,
			run_at, status, message, total, success_count, fail_count, duration_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.PlanID, r.PlanName, r.TaskType, r.TargetType, r.AccountID,
		r.RunAt, r.Status, r.Message, r.Total, r.SuccessCount, r.FailCount, r.DurationMs)
	return err
}

func (db *DB) ListPlanRunRecords(limit int) ([]*PlanRunRecord, error) {
	rows, err := db.conn.Query(`
		SELECT id, plan_id, plan_name, task_type, target_type, account_id, run_at,
		       status, message, total, success_count, fail_count, duration_ms
		FROM plan_run_records ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PlanRunRecord
	for rows.Next() {
		var r PlanRunRecord
		if err := rows.Scan(&r.ID, &r.PlanID, &r.PlanName, &r.TaskType, &r.TargetType,
			&r.AccountID, &r.RunAt, &r.Status, &r.Message, &r.Total,
			&r.SuccessCount, &r.FailCount, &r.DurationMs); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
