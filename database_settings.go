package main

import (
	"database/sql"
	"time"
)

// ---- 设置 KV ----

// settingsCacheTTL 设置读缓存时长：热路径（每请求多次 GetSetting）与外部
// 直改 sqlite 的可见性之间的折中
const settingsCacheTTL = 3 * time.Second

type settingsCacheEntry struct {
	value string
	at    time.Time
}

// vaultSecretSettingsSet 机密设置键集合：落库前 vault 加密，读取后透明解密
var vaultSecretSettingsSet = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range vaultSecretSettings {
		m[k] = true
	}
	return m
}()

// GetSetting 读取设置项（机密键透明解密；普通键带 3s 进程内读缓存）。
// vault 机密键不缓存：密钥轮换/降级语义要求每次真实解密，
// 缓存明文会让"错钥匙读到空"的守卫失效。
func (db *DB) GetSetting(key string) (string, error) {
	if !vaultSecretSettingsSet[key] {
		db.setMu.Lock()
		if db.setCache != nil {
			if e, ok := db.setCache[key]; ok && time.Since(e.at) < settingsCacheTTL {
				db.setMu.Unlock()
				return e.value, nil
			}
		}
		db.setMu.Unlock()
	}

	var v string
	err := db.conn.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		if !vaultSecretSettingsSet[key] {
			db.cacheSetting(key, "")
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if v != "" && vaultSecretSettingsSet[key] {
		return vaultDecrypt(v), nil
	}
	db.cacheSetting(key, v)
	return v, nil
}

// cacheSetting 更新设置读缓存
func (db *DB) cacheSetting(key, value string) {
	db.setMu.Lock()
	if db.setCache == nil {
		db.setCache = map[string]settingsCacheEntry{}
	}
	db.setCache[key] = settingsCacheEntry{value: value, at: time.Now()}
	db.setMu.Unlock()
}

// SetSetting 写入设置项（机密键落库前加密；加密失败中止写入；成功后同步读缓存）
func (db *DB) SetSetting(key, value string) error {
	if value != "" && vaultSecretSettingsSet[key] {
		// 统一走 vaultEncrypt：加密明文；带 vault1: 前缀的原值仅在其为当前钥匙
		// 可解密文时透传（幂等再写）。绕过它直写解不开的脏密文会永久占位、
		// 读回恒空，网关认证静默失效直到手工修库
		enc, err := vaultEncrypt(value)
		if err != nil {
			return err
		}
		value = enc
	}
	_, err := db.conn.Exec(`
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = datetime('now','localtime')`,
		key, value)
	if err == nil && !vaultSecretSettingsSet[key] {
		// 明文进缓存（与 GetSetting 返回一致；机密键不缓存）
		db.cacheSetting(key, value)
	} else if err != nil {
		// 写失败作废缓存，避免旧值滞留
		db.setMu.Lock()
		delete(db.setCache, key)
		db.setMu.Unlock()
	}
	return err
}

// HasSetting 判断设置项是否存在（读原始值，不解密——用于区分
// "未设置"与"存在但当前钥匙解不开"）
func (db *DB) HasSetting(key string) (bool, error) {
	var v string
	err := db.conn.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AllSettings 返回全部设置
func (db *DB) AllSettings() (map[string]string, error) {
	rows, err := db.conn.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err == nil {
			out[k] = v
		}
	}
	return out, rows.Err()
}

// ---- 认证相关设置 ----

func (db *DB) GetPasswordHash() (string, error)  { return db.GetSetting("password_hash") }
func (db *DB) SetPasswordHash(hash string) error { return db.SetSetting("password_hash", hash) }

// IsDefaultPassword 是否仍使用引导期生成的初始口令。
// 标记缺失（老库/未写）一律视为 false——缺省告警只信任显式写入的标记，
// 避免 env 口令部署与升级库被误报警。
func (db *DB) IsDefaultPassword() (bool, error) {
	v, err := db.GetSetting("is_default_password")
	if err != nil || v == "" {
		return false, err
	}
	return v == "1", nil
}

func (db *DB) SetDefaultPasswordFlag(isDefault bool) error {
	v := "0"
	if isDefault {
		v = "1"
	}
	return db.SetSetting("is_default_password", v)
}

func (db *DB) GetAPIKey() (string, error) { return db.GetSetting("api_key") }
func (db *DB) SetAPIKey(key string) error { return db.SetSetting("api_key", key) }

func (db *DB) GetAdminUser() (string, error) {
	u, err := db.GetSetting("admin_user")
	if err != nil || u == "" {
		return "admin", err
	}
	return u, nil
}
