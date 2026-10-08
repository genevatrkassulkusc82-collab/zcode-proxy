package main

import (
	"database/sql"
	"strings"
	"time"
)

// ---- 下游网关 Key（R1）----
// 每客户端独立 sk- Key：名称 / 启停 / RPM 限速 / 总 token 配额 / 模型白名单。
// 明文只在创建响应里出现一次，库内只存 SHA-256；旧的单把 api_key 保持不变，
// 作为不受限的根 Key 与命名 Key 并行有效。

type GatewayKey struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	KeyHash    string `json:"-"`            // sha256 hex（明文不落库）
	KeyPrefix  string `json:"key_prefix"`   // 展示用前缀，如 sk-ab12…
	Enabled    bool   `json:"enabled"`
	RPMLimit   int    `json:"rpm_limit"`    // 每分钟请求数上限，0 = 不限
	QuotaTotal int64  `json:"quota_total"`  // 总 token 配额（prompt+completion），0 = 不限
	QuotaUsed  int64  `json:"quota_used"`   // 已用 token（usage 落库时累加）
	Models     string `json:"models"`       // 逗号分隔白名单（小写），空 = 不限
	LastUsedAt int64  `json:"last_used_at"` // epoch 秒
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`

	Key string `json:"key,omitempty"` // 仅 CreateGatewayKey 返回时填充明文
}

const gatewayKeyCols = `id, name, key_hash, key_prefix, enabled, rpm_limit,
	quota_total, quota_used, models, last_used_at, created_at, updated_at`

func scanGatewayKey(row interface{ Scan(...interface{}) error }) (*GatewayKey, error) {
	var k GatewayKey
	var enabled int
	if err := row.Scan(&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &enabled, &k.RPMLimit,
		&k.QuotaTotal, &k.QuotaUsed, &k.Models, &k.LastUsedAt, &k.CreatedAt, &k.UpdatedAt); err != nil {
		return nil, err
	}
	k.Enabled = enabled == 1
	return &k, nil
}

// CreateGatewayKey 插入新 Key；k.KeyHash 须已由调用方算好，k.Key 携带明文原样返回。
func (db *DB) CreateGatewayKey(k *GatewayKey) (int64, error) {
	res, err := db.conn.Exec(`
		INSERT INTO gateway_keys (name, key_hash, key_prefix, enabled, rpm_limit,
			quota_total, quota_used, models)
		VALUES (?,?,?,?,?,?,0,?)`,
		k.Name, k.KeyHash, k.KeyPrefix, boolInt(k.Enabled), k.RPMLimit,
		k.QuotaTotal, normalizeModelWhitelist(k.Models))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) ListGatewayKeys() ([]*GatewayKey, error) {
	rows, err := db.conn.Query(`SELECT ` + gatewayKeyCols + ` FROM gateway_keys ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*GatewayKey
	for rows.Next() {
		k, err := scanGatewayKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetGatewayKeyByHash 认证路径：按 SHA-256 摘要取 Key
func (db *DB) GetGatewayKeyByHash(hash string) (*GatewayKey, error) {
	row := db.conn.QueryRow(`SELECT `+gatewayKeyCols+` FROM gateway_keys WHERE key_hash = ?`, hash)
	k, err := scanGatewayKey(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// GetGatewayKey 管理路径：按 ID
func (db *DB) GetGatewayKey(id int64) (*GatewayKey, error) {
	row := db.conn.QueryRow(`SELECT `+gatewayKeyCols+` FROM gateway_keys WHERE id = ?`, id)
	k, err := scanGatewayKey(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// UpdateGatewayKeyFields 编辑可变字段；models 归一化为小写白名单
func (db *DB) UpdateGatewayKeyFields(id int64, name string, enabled bool, rpmLimit int, quotaTotal int64, models string) error {
	_, err := db.conn.Exec(`
		UPDATE gateway_keys SET name=?, enabled=?, rpm_limit=?, quota_total=?, models=?,
		updated_at=datetime('now','localtime') WHERE id=?`,
		name, boolInt(enabled), rpmLimit, quotaTotal, normalizeModelWhitelist(models), id)
	return err
}

func (db *DB) DeleteGatewayKey(id int64) error {
	_, err := db.conn.Exec(`DELETE FROM gateway_keys WHERE id = ?`, id)
	return err
}

// BumpGatewayKeyUsage usage 落库后累加配额消耗（finalizer 语义：超限在下一次请求前拦截）
func (db *DB) BumpGatewayKeyUsage(id int64, tokens int) {
	if id <= 0 {
		return
	}
	db.conn.Exec(`UPDATE gateway_keys SET quota_used = quota_used + ?,
		last_used_at = ?, updated_at = updated_at WHERE id = ?`, tokens, time.Now().Unix(), id)
}

// normalizeModelWhitelist 白名单归一化：trim + 小写 + 去空项 + 去重，保序
func normalizeModelWhitelist(models string) string {
	seen := map[string]bool{}
	var out []string
	for _, m := range strings.Split(models, ",") {
		m = strings.ToLower(strings.TrimSpace(m))
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return strings.Join(out, ",")
}

// modelAllowed 白名单校验：空 = 全部放行；model 已去 provider 前缀并小写
func (k *GatewayKey) modelAllowed(model string) bool {
	if k.Models == "" {
		return true
	}
	for _, m := range strings.Split(k.Models, ",") {
		if strings.TrimSpace(m) == model {
			return true
		}
	}
	return false
}
