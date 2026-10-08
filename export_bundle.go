package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"golang.org/x/crypto/pbkdf2"
)

// ---- 加密账号包导出 / 导入 ----
// 格式: "zcb1:" + base64( salt(16) || nonce(12) || ciphertext )
// 密钥: PBKDF2-SHA256(password, salt, 600000, 32) → AES-256-GCM（旧版包为 120000 轮）
// 用于跨机器迁移账号（含 JWT / API Key / 设备指纹 / 凭证快照）。

const bundlePrefix = "zcb1:"

const (
	// 新导出使用 600k 轮次；旧版 120k 的包经 legacy 路径兼容导入
	pbkdf2Iterations       = 600000
	legacyPBKDF2Iterations = 120000
)

// bundleAccount 包内账号结构（不含内部 ID，导入时按 user_id upsert）
type bundleAccount struct {
	UserID       string `json:"user_id"`
	Email        string `json:"email"`
	DisplayName  string `json:"display_name"`
	Provider     string `json:"provider"`
	AuthType     string `json:"auth_type"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ZCodeJWT     string `json:"zcode_jwt,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	UserInfo     string `json:"user_info,omitempty"`
	DeviceMid    string `json:"device_mid,omitempty"`
	CredsRaw     string `json:"creds_raw,omitempty"`
	AccountGroup string `json:"account_group,omitempty"`
	Remark       string `json:"remark,omitempty"`
}

// bundleGCM PBKDF2-SHA256 派生密钥并构建 AES-256-GCM
func bundleGCM(password string, salt []byte, iterations int) (cipher.AEAD, error) {
	key := pbkdf2.Key([]byte(password), salt, iterations, 32, sha256.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ExportBundle 导出全部（或指定）账号为加密包字符串
func (m *AccountManager) ExportBundle(password string, ids []int64) (string, error) {
	if password == "" {
		return "", fmt.Errorf("请设置导出密码")
	}
	// 库中存在解不开的密文时拒绝导出：解密失败会静默变空串（omitempty 直接
	// 丢字段），导出的包看似成功实则缺凭证——恰是用户拿去迁移的最坏时刻。
	if _, broken, err := m.db.scanVaultCiphertext(currentVaultSeed()); err != nil {
		return "", fmt.Errorf("凭证完整性检查失败: %w", err)
	} else if broken > 0 {
		return "", fmt.Errorf("库中有 %d 条凭证无法用当前钥匙解密（ZCODE_PROXY_VAULT_SECRET 或 vault.key 不匹配）。该检查覆盖全库（含未勾选账号），以避免导出包静默缺凭证；请先恢复钥匙再导出", broken)
	}
	all, err := m.db.ListAccounts("")
	if err != nil {
		return "", err
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var items []bundleAccount
	for _, a := range all {
		if len(want) > 0 && !want[a.ID] {
			continue
		}
		items = append(items, bundleAccount{
			UserID: a.UserID, Email: a.Email, DisplayName: a.DisplayName,
			Provider: a.Provider, AuthType: a.AuthType,
			AccessToken: a.AccessToken, RefreshToken: a.RefreshToken,
			ZCodeJWT: a.ZCodeJWT, APIKey: a.APIKey, UserInfo: a.UserInfo,
			DeviceMid: a.DeviceMid, CredsRaw: a.CredsRaw,
			AccountGroup: a.AccountGroup, Remark: a.Remark,
		})
	}
	if len(items) == 0 {
		return "", fmt.Errorf("没有可导出的账号")
	}
	plain, err := json.Marshal(map[string]interface{}{"version": 1, "accounts": items})
	if err != nil {
		return "", err
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	gcm, err := bundleGCM(password, salt, pbkdf2Iterations)
	if err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, plain, nil)

	raw := append(append(append([]byte{}, salt...), nonce...), ct...)
	return bundlePrefix + base64.StdEncoding.EncodeToString(raw), nil
}

// ImportBundle 解密并导入账号包，返回导入账号数
func (m *AccountManager) ImportBundle(password, bundle string) (int, error) {
	bundle = strings.TrimSpace(bundle)
	if !strings.HasPrefix(bundle, bundlePrefix) {
		return 0, fmt.Errorf("不是有效的账号包（缺少 zcb1: 前缀）")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(bundle, bundlePrefix))
	if err != nil {
		return 0, fmt.Errorf("账号包 base64 解码失败")
	}
	if len(raw) < 16+12+16 {
		return 0, fmt.Errorf("账号包数据过短")
	}
	salt, nonce, ct := raw[:16], raw[16:28], raw[28:]
	gcm, err := bundleGCM(password, salt, pbkdf2Iterations)
	if err != nil {
		return 0, err
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// 新参数解密失败：旧版 120k 轮次的包经 legacy 路径重试导入
		gcm, err = bundleGCM(password, salt, legacyPBKDF2Iterations)
		if err != nil {
			return 0, err
		}
		if plain, err = gcm.Open(nil, nonce, ct, nil); err != nil {
			return 0, fmt.Errorf("解密失败：密码错误或包已损坏")
		}
	}
	var payload struct {
		Version  int             `json:"version"`
		Accounts []bundleAccount `json:"accounts"`
	}
	if err := json.Unmarshal(plain, &payload); err != nil {
		return 0, fmt.Errorf("包内容解析失败")
	}
	// 明确拒绝未知版本：未来字段结构变化时不得静默错解
	if payload.Version > 1 {
		return 0, fmt.Errorf("不支持的账号包版本 %d（当前最高支持 v1，请升级本网关）", payload.Version)
	}
	count := 0
	for _, it := range payload.Accounts {
		if it.UserID == "" || (it.ZCodeJWT == "" && it.APIKey == "") {
			continue
		}
		a := &Account{
			UserID: it.UserID, Email: it.Email, DisplayName: it.DisplayName,
			Provider: firstNonEmpty(it.Provider, "zai"), AuthType: firstNonEmpty(it.AuthType, "jwt"),
			AccessToken: it.AccessToken, RefreshToken: it.RefreshToken,
			ZCodeJWT: it.ZCodeJWT, APIKey: it.APIKey, UserInfo: it.UserInfo,
			DeviceMid: it.DeviceMid, CredsRaw: it.CredsRaw,
			AccountGroup: it.AccountGroup, Remark: it.Remark,
			Status: StatusActive, Enabled: true,
		}
		if _, err := m.db.UpsertAccount(a); err != nil {
			// 静默跳过会让导入方以为全部成功（如 vault 拒写的脏字段）
			log.Printf("[bundle] import %s skipped: %v", it.UserID, err)
			continue
		}
		count++
	}
	return count, nil
}
