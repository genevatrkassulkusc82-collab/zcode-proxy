package main

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"sort"
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

// HasResetRecordNear 是否已存在该账号邻近时间内、同一重置类型（kind）的成功记录
// （用于上游 used_at 去重：官方客户端等外部执行的重置不必重复入库。
// 必须按 kind 区分：five_hour 与 week 背靠背消耗时互不构成重复）
// 窗口分三档：
//   - used_at 精确相等：同为上游时钟的同步行；
//   - used_at ±900s 邻近：本地执行行的 used_at 是本地时钟、同步行是上游时钟，
//     两级延迟下秒级相等是赌博；
//   - used_at=0（遗留行）走 created_at 墙钟回退 ±5400s：created_at 是
//     localtime 墙钟串，strftime('%s') 按 UTC 解析，查询端只有"今天"的 UTC
//     偏移可还原 epoch；跨越夏令时边界的行偏移差最大 3600s，±900s 会漏配——
//     漏配 = 对同一窗口重复消耗重置，比误配（多跳过一次重复写入）危险得多。
//     HK 生产无 DST 不受影响，此窗仅覆盖跨时区/历史数据。
func (db *DB) HasResetRecordNear(accountID int64, usedAtSec int64, kind string) (bool, error) {
	_, offset := time.Now().Zone()
	var n int
	err := db.conn.QueryRow(
		`SELECT COUNT(1) FROM claim_records
		 WHERE account_id=? AND task_type='reset' AND success=1
		   AND plan_name LIKE ?
		   AND (used_at = ?
		        OR (used_at > 0 AND ABS(used_at - ?) < 900)
		        OR (used_at = 0 AND ABS(strftime('%s',created_at)-?-?)<5400))`,
		accountID, "%("+kind+")%", usedAtSec, usedAtSec, offset, usedAtSec).Scan(&n)
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
	// 分节失败不得静默洗成空数据（如大清库 DELETE 持连接期间的 SQLITE_BUSY
	// 会把整个趋势图渲染成"零用量"）：逐节收集错误，返回首个失败供上层警示
	sectionErrs := map[string]string{}
	var firstErr error
	noteErr := func(section string, err error) {
		if err == nil {
			return
		}
		sectionErrs[section] = err.Error()
		if firstErr == nil {
			firstErr = fmt.Errorf("usage stats section %s: %w", section, err)
		}
	}
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
	// TTFT 分位（非零样本；水库采样封顶 10 万条，任意窗口大小下均无偏）
	if ttfts, err := db.ttftSamples(since, 100000); err == nil && len(ttfts) > 0 {
		out["p50_ttft_ms"] = percentile(ttfts, 0.5)
		out["p95_ttft_ms"] = percentile(ttfts, 0.95)
	} else {
		noteErr("ttft", err)
	}

	// 按模型分布
	models := map[string]int{}
	rows, err := db.conn.Query(`SELECT model, COUNT(*) FROM usage_records WHERE created_at >= ? GROUP BY model`, since)
	if err != nil {
		noteErr("by_model", err)
	} else {
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
	if err != nil {
		noteErr("by_account", err)
	} else {
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
	if err != nil {
		noteErr("by_gateway_key", err)
	} else {
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
	if err != nil {
		noteErr("daily", err)
	} else {
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
	if firstErr != nil {
		// 部分成功：保留已取到的分节数据，附 stats_errors 供上层区分
		// "零用量"与"查询失败"，并返回首个失败让调用方决定如何告警
		out["stats_errors"] = sectionErrs
		return out, firstErr
	}
	return out, nil
}

// ttftSamples 窗口内非零 TTFT 样本（升序返回，供分位计算）。
// 不用 SQL ORDER BY ttft_ms LIMIT cap：那取的是"最快尾部的 cap 条"，
// 窗口样本数一旦超过 cap，p50/p95 会整体向快端静默漂移。改为全量流式
// 扫描 + 水库采样（Algorithm R）封顶 cap 条：对任意窗口大小均无偏，
// 内存有界（cap=100000 约 0.8MB）。
func (db *DB) ttftSamples(since string, cap int) ([]int, error) {
	if cap <= 0 {
		return nil, nil
	}
	rows, err := db.conn.Query(`
		SELECT ttft_ms FROM usage_records
		WHERE created_at >= ? AND ttft_ms > 0`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sample := make([]int, 0, cap)
	seen := 0
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			continue
		}
		if len(sample) < cap {
			sample = append(sample, v)
		} else if j := rand.IntN(seen + 1); j < cap {
			// 第 seen+1 条样本以 cap/(seen+1) 概率换掉已采样的随机槽位
			sample[j] = v
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Ints(sample)
	return sample, nil
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
		// 下次保存时加密迁移）。解不开标记 PasswordBroken——静默变空串
		// 会让带用户名的节点用空密码拨号、整组账号连环冷却
		var ok bool
		n.Password, ok = vaultDecryptOK(n.Password)
		n.PasswordBroken = !ok
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
// 密码密文解不开（PasswordBroken）的节点跳过：带用户名却用空密码拨号必被
// 代理拒绝，整组账号会连环冷却——跳过让组回退默认节点/直连，面板可见告警。
func (db *DB) ProxyNodeForGroup(group string) (*ProxyNode, error) {
	nodes, err := db.ListProxyNodes()
	if err != nil {
		return nil, err
	}
	unusable := func(n *ProxyNode) bool {
		return !n.Enabled || (n.PasswordBroken && n.Username != "")
	}
	if group != "" {
		for _, n := range nodes {
			if unusable(n) {
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
		if n.Enabled && n.IsDefault && !unusable(n) {
			return n, nil
		}
	}
	return nil, nil
}

// ---- 记录保留（防无界增长：stats 聚合随表龄线性变慢）----

// PruneRecords 删除时间戳（usage_records/claim_records 按 created_at，
// plan_run_records 按 run_at）早于 days 天的记录行，返回三表删除总数。
// claim_records（自动领取每账号每轮一行）与 plan_run_records（分钟级计划
// 一天 1440 行）与 usage_records 同样无界增长，共用同一个保留旋钮。
// days <= 0 = 永久保留（不执行）
func (db *DB) PruneRecords(days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02 15:04:05")
	var total int64
	for _, stmt := range []string{
		`DELETE FROM usage_records WHERE created_at < ?`,
		`DELETE FROM claim_records WHERE created_at < ?`,
		`DELETE FROM plan_run_records WHERE run_at < ?`,
	} {
		res, err := db.conn.Exec(stmt, cutoff)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// UsageRetentionDays 记录保留天数（覆盖 usage_records / claim_records /
// plan_run_records 三表）：未配置 = 90；显式 0 = 永久保留
func (db *DB) UsageRetentionDays() int {
	v, _ := db.GetSetting("usage_retention_days")
	if v == "" {
		return 90
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return n
	}
	return 90
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
