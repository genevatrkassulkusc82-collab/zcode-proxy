package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ---- 账号管理：本地客户端导入 / 粘贴导入 / 一键切回 ----

// AccountManager 账号来源管理
type AccountManager struct {
	db    *DB
	zapi  *ZCodeAPI
	oauth *OAuthManager
}

// NewAccountManager 创建账号管理器
func NewAccountManager(db *DB, zapi *ZCodeAPI, oauth *OAuthManager) *AccountManager {
	return &AccountManager{db: db, zapi: zapi, oauth: oauth}
}

// ---- 本地客户端导入 ----

// localClientFiles 本地 ZCode 客户端相关文件路径
type localClientFiles struct {
	home        string
	credentials string // ~/.zcode/v2/credentials.json
	config      string // ~/.zcode/v2/config.json
	telemetry   string // ~/.zcode/v2/telemetry-state.json
	cache       string // ~/.zcode/v2/coding-plan-cache.json
}

func localClientFilesFor(home string) localClientFiles {
	v2 := filepath.Join(home, ".zcode", "v2")
	return localClientFiles{
		home:        home,
		credentials: filepath.Join(v2, "credentials.json"),
		config:      filepath.Join(v2, "config.json"),
		telemetry:   filepath.Join(v2, "telemetry-state.json"),
		cache:       filepath.Join(v2, "coding-plan-cache.json"),
	}
}

func resolveLocalClientFiles() localClientFiles {
	home, _ := os.UserHomeDir()
	return localClientFilesFor(home)
}

// ImportFromLocalClient 从本机 ZCode 客户端导入当前登录账号，
// 并顺带扫描多实例凭证目录（~/.zcode-multi、macOS Application Support）。
func (m *AccountManager) ImportFromLocalClient(group string) (*Account, error) {
	a, err := m.importLocalClient(resolveLocalClientFiles(), group, "本地客户端导入")
	if err != nil {
		return nil, err
	}
	// 多实例扫描失败不影响主导入（fail-soft）
	scanned, found, imported := m.ImportMultiInstanceClients(group)
	if scanned > 0 {
		log.Printf("[import] multi-instance scan: scanned=%d found=%d imported=%d", scanned, found, imported)
	}
	return a, nil
}

// ImportMultiInstanceClients 扫描多实例 ZCode 客户端凭证并导入。
// 布局：<home>/.zcode-multi/<实例>/v2/credentials.json，darwin 另有
// ~/Library/Application Support/zcode-multi/<实例>/v2/credentials.json。
// 每个实例走与主导入完全相同的解密/导入路径（实例目录即密钥推导 home）；
// user_id 自然键 upsert 使重复导入幂等（同一用户多实例自然合并）。
// 返回：扫描的实例数 / 含可解析凭证的实例数 / 成功入库的实例数。
func (m *AccountManager) ImportMultiInstanceClients(group string) (scanned, found, imported int) {
	home, _ := os.UserHomeDir()
	patterns := []string{
		filepath.Join(home, ".zcode-multi", "*", "v2", "credentials.json"),
	}
	if runtime.GOOS == "darwin" {
		patterns = append(patterns,
			filepath.Join(home, "Library", "Application Support", "zcode-multi", "*", "v2", "credentials.json"))
	}
	seen := map[string]bool{}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern) // glob 出错按零结果处理（fail-soft）
		if err != nil {
			continue
		}
		for _, credPath := range matches {
			if seen[credPath] {
				continue
			}
			seen[credPath] = true
			scanned++
			data, err := os.ReadFile(credPath)
			if err != nil {
				continue
			}
			var creds map[string]string
			if json.Unmarshal(data, &creds) != nil || len(creds) == 0 {
				continue
			}
			found++
			// credPath = <实例>/v2/credentials.json，v2 上级即实例目录
			v2Dir := filepath.Dir(credPath)
			instanceDir := filepath.Dir(v2Dir)
			f := localClientFiles{
				home:        instanceDir, // 多实例凭证以实例目录推导密钥
				credentials: credPath,
				config:      filepath.Join(v2Dir, "config.json"),
				telemetry:   filepath.Join(v2Dir, "telemetry-state.json"),
				cache:       filepath.Join(v2Dir, "coding-plan-cache.json"),
			}
			if _, err := m.importLocalClient(f, group,
				"多实例导入: "+filepath.Base(instanceDir)); err != nil {
				log.Printf("[import] instance %s: %v", filepath.Base(instanceDir), err)
				continue
			}
			imported++
		}
	}
	return scanned, found, imported
}

// importLocalClient 从给定 localClientFiles（凭证/配置/遥测文件组）导入账号。
// 解密 credentials.json 提取 JWT/access_token/user_info；
// 从 config.json 提取 coding-plan API Key；保留原始文件内容供一键切回。
// f.home 仅用作凭证密钥推导基准（主导入=用户主目录，多实例=实例目录）。
func (m *AccountManager) importLocalClient(f localClientFiles, group, remark string) (*Account, error) {
	credData, err := os.ReadFile(f.credentials)
	if err != nil {
		return nil, fmt.Errorf("读取本地凭证失败（ZCode 客户端可能未安装/未登录）: %w", err)
	}
	var creds map[string]string
	if err := json.Unmarshal(credData, &creds); err != nil {
		return nil, fmt.Errorf("凭证文件解析失败: %w", err)
	}

	secret := DefaultCredentialSecret(f.home)
	dec := func(key string) string {
		v, ok := creds[key]
		if !ok {
			return ""
		}
		plain, err := DecryptCredential(v, secret)
		if err != nil {
			log.Printf("[import] decrypt %s: %v", key, err)
			return ""
		}
		return plain
	}

	provider := dec("oauth:active_provider")
	if provider == "" {
		provider = "zai"
	}
	zcodeJWT := dec("zcodejwttoken")
	accessToken := dec(fmt.Sprintf("oauth:%s:access_token", provider))
	userInfo := dec(fmt.Sprintf("oauth:%s:user_info", provider))
	if zcodeJWT == "" && accessToken == "" {
		return nil, fmt.Errorf("本地凭证中没有可用的 ZCode 登录态（请先在 ZCode 客户端登录）")
	}

	// user_info → email/name/user_id
	var ui map[string]interface{}
	if userInfo != "" {
		json.Unmarshal([]byte(userInfo), &ui)
	}
	email := jsonStr(ui, "email")
	displayName := firstNonEmpty(jsonStr(ui, "name"), jsonStr(ui, "username"), jsonStr(ui, "displayName"))
	userID := firstNonEmpty(jsonStr(ui, "user_id"), jsonStr(ui, "id"))
	if userID == "" && zcodeJWT != "" {
		if claims, err := DecodeJWTPayload(zcodeJWT); err == nil {
			userID = firstNonEmpty(jsonStr(claims, "user_id"), jsonStr(claims, "sub"))
		}
	}
	if userID == "" && accessToken != "" {
		if claims, err := DecodeJWTPayload(accessToken); err == nil {
			userID = firstNonEmpty(jsonStr(claims, "user_id"), jsonStr(claims, "sub"))
		}
	}
	if userID == "" {
		return nil, fmt.Errorf("无法确定账号 user_id")
	}

	// device_mid
	deviceMid := ""
	if tData, err := os.ReadFile(f.telemetry); err == nil {
		var t struct {
			DeviceMid string `json:"deviceMid"`
		}
		if json.Unmarshal(tData, &t) == nil {
			deviceMid = t.DeviceMid
		}
	}

	// config.json → coding-plan API Key（已是 {id}.{secret} 完整格式）
	apiKey := ""
	if cData, err := os.ReadFile(f.config); err == nil {
		var cfg struct {
			Provider map[string]struct {
				Enabled *bool `json:"enabled"`
				Options struct {
					APIKey string `json:"apiKey"`
				} `json:"options"`
			} `json:"provider"`
		}
		if json.Unmarshal(cData, &cfg) == nil {
			for id, p := range cfg.Provider {
				if strings.Contains(id, "coding-plan") && strings.Contains(id, provider) &&
					p.Options.APIKey != "" && !strings.HasPrefix(p.Options.APIKey, "enc:") {
					apiKey = p.Options.APIKey
					break
				}
			}
		}
	}

	// 原始凭证快照（供一键切回还原）
	snapshot := map[string]string{
		"credentials.json": string(credData),
	}
	if cData, err := os.ReadFile(f.config); err == nil {
		snapshot["config.json"] = string(cData)
	}
	snapshotJSON, _ := json.Marshal(snapshot)

	a := &Account{
		UserID:       userID,
		Email:        email,
		DisplayName:  displayName,
		Provider:     provider,
		AuthType:     "jwt",
		AccessToken:  accessToken,
		ZCodeJWT:     zcodeJWT,
		APIKey:       apiKey,
		UserInfo:     userInfo,
		DeviceMid:    deviceMid,
		CredsRaw:     string(snapshotJSON),
		Status:       StatusActive,
		Enabled:      true,
		AccountGroup: group,
		Remark:       remark,
	}
	if zcodeJWT == "" {
		a.AuthType = "apikey"
	}
	id, err := m.db.UpsertAccount(a)
	if err != nil {
		return nil, fmt.Errorf("账号入库失败: %w", err)
	}
	a.ID = id
	m.ensureAccountIdentity(a)
	log.Printf("[import] local client account imported: %s (id=%d, jwt=%v, apikey=%v)",
		email, id, zcodeJWT != "", apiKey != "")

	// 异步刷新额度：在库内新副本上刷新——本副本随即被 handler 无锁序列化
	// （accountPublicView），共享实例就地写会撕裂字符串字段（-race/segfault 面）
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[import] quota refresh panic: %v", r)
			}
		}()
		time.Sleep(500 * time.Millisecond)
		fresh, err := m.db.GetAccount(id)
		if err != nil {
			log.Printf("[import] quota refresh %s: %v", email, err)
			return
		}
		if err := m.zapi.RefreshAccountQuota(fresh); err != nil {
			log.Printf("[import] quota refresh %s: %v", email, err)
		}
	}()
	return a, nil
}

// ---- 粘贴导入 ----

// LooksLikeJWT 判断凭证是否为 JWT 形状（3 段 base64url）
func LooksLikeJWT(secret string) bool {
	parts := strings.Split(strings.TrimSpace(secret), ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, c := range p {
			if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", c) {
				return false
			}
		}
	}
	return true
}

// ImportPasted 粘贴 JWT 或 API Key 导入
func (m *AccountManager) ImportPasted(provider, name, secret, group string) (*Account, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, fmt.Errorf("凭证不能为空")
	}
	if provider == "" {
		provider = "zai"
	}
	isJWT := LooksLikeJWT(secret) && provider == "zai"

	// user_id：JWT 解 payload；API Key 用哈希做自然键
	userID := ""
	if isJWT {
		if claims, err := DecodeJWTPayload(secret); err == nil {
			userID = firstNonEmpty(jsonStr(claims, "user_id"), jsonStr(claims, "sub"))
			// 过期 JWT 照常导入（管理员可能手上有刷新材料），但导入时如实告警
			if exp, ok := claims["exp"].(float64); ok && exp > 0 && time.Now().Unix() >= int64(exp) {
				log.Printf("[import] WARNING: pasted JWT already expired at %s — first relay request will 401 until refreshed",
					time.Unix(int64(exp), 0).Format("2006-01-02 15:04"))
			}
		}
	}
	if userID == "" {
		h := sha256.Sum256([]byte(provider + ":" + secret))
		userID = provider + "-" + hex.EncodeToString(h[:8])
	}

	a := &Account{
		UserID:       userID,
		DisplayName:  strings.TrimSpace(name),
		Email:        "",
		Provider:     provider,
		Status:       StatusActive,
		Enabled:      true,
		AccountGroup: group,
		Remark:       "粘贴导入",
	}
	if isJWT {
		a.AuthType = "jwt"
		a.ZCodeJWT = secret
	} else {
		a.AuthType = "apikey"
		a.APIKey = secret
	}
	id, err := m.db.UpsertAccount(a)
	if err != nil {
		return nil, fmt.Errorf("账号入库失败: %w", err)
	}
	a.ID = id
	m.ensureAccountIdentity(a)

	// 异步刷新额度：与本地导入同纪律——在库内新副本上刷新，本副本随即被
	// handler 无锁序列化（accountPublicView），共享实例并发写会撕裂字符串字段
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[import] paste quota refresh panic: %v", r)
			}
		}()
		time.Sleep(500 * time.Millisecond)
		fresh, err := m.db.GetAccount(id)
		if err != nil {
			return
		}
		m.zapi.RefreshAccountQuota(fresh)
	}()
	return a, nil
}

// ---- 一键切回本地客户端 ----

// SwitchBackToLocal 把所选账号凭证写回本地 ZCode 客户端：
//  1. 快照现有 credentials.json / config.json 到 data/backups/
//  2. 重新 enc:v1 加密写回 credentials.json（原子替换）
//  3. 更新 config.json 的 start-plan / coding-plan apiKey 与 enabled
//  4. 删除 coding-plan-cache.json 强制客户端重新探测套餐
func (m *AccountManager) SwitchBackToLocal(accountID int64, killClient bool) error {
	a, err := m.db.GetAccount(accountID)
	if err != nil {
		return err
	}
	if a.ZCodeJWT == "" {
		return fmt.Errorf("该账号没有 ZCode JWT，无法切回本地客户端")
	}
	f := resolveLocalClientFiles()
	secret := DefaultCredentialSecret(f.home)

	// 1. 备份（基于可执行文件目录，避免受工作目录影响）。
	// 备份是切回的唯一可逆手段：备份失败必须中止，不得先覆盖线上凭证
	backupDir := filepath.Join(exeDir(), "data", "backups")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return fmt.Errorf("创建备份目录失败: %w", err)
	}
	stamp := time.Now().Format("20060102-150405")
	for _, p := range []string{f.credentials, f.config} {
		if data, err := os.ReadFile(p); err == nil {
			// 备份含凭证快照，限权 0600（WriteFile 对已存在文件不改权限，补一次 Chmod）
			bak := filepath.Join(backupDir, filepath.Base(p)+"."+stamp+".bak")
			if err := os.WriteFile(bak, data, 0600); err != nil {
				return fmt.Errorf("备份 %s 失败: %w", filepath.Base(p), err)
			}
			if err := os.Chmod(bak, 0600); err != nil {
				return fmt.Errorf("收紧备份权限失败: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("读取 %s 备份失败: %w", filepath.Base(p), err)
		}
	}

	// 2. 重建 credentials.json：保留无关键（bot/web-remote-control 等），只替换登录态
	var creds map[string]string
	if data, err := os.ReadFile(f.credentials); err == nil {
		if err := json.Unmarshal(data, &creds); err != nil {
			return fmt.Errorf("解析 credentials.json 失败: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取 credentials.json 失败: %w", err)
	}
	if creds == nil {
		creds = map[string]string{}
	}
	// 配置也先读完校验，避免解析失败后已经切换部分身份。
	var cfg map[string]interface{}
	if data, err := os.ReadFile(f.config); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("解析 config.json 失败: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取 config.json 失败: %w", err)
	}
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	provider := a.Provider
	if provider == "" {
		provider = "zai"
	}
	enc := func(plain string) (string, error) { return EncryptCredential(plain, secret) }

	// 全部加密成功才动笔：部分成功会写出一个"旧登录态 + 新计费键"的混合身份
	var zcodeEnc, accessEnc, userEnc, providerEnc string
	var encErr error
	if zcodeEnc, encErr = enc(a.ZCodeJWT); encErr != nil {
		return fmt.Errorf("加密 zcodejwttoken 失败: %w", encErr)
	}
	if a.AccessToken != "" {
		if accessEnc, encErr = enc(a.AccessToken); encErr != nil {
			return fmt.Errorf("加密 access_token 失败: %w", encErr)
		}
	}
	if a.UserInfo != "" {
		if userEnc, encErr = enc(a.UserInfo); encErr != nil {
			return fmt.Errorf("加密 user_info 失败: %w", encErr)
		}
	}
	if providerEnc, encErr = enc(provider); encErr != nil {
		return fmt.Errorf("加密 provider 失败: %w", encErr)
	}

	creds["zcodejwttoken"] = zcodeEnc
	if accessEnc != "" {
		creds["oauth:"+provider+":access_token"] = accessEnc
	} else {
		delete(creds, "oauth:"+provider+":access_token")
	}
	if userEnc != "" {
		creds["oauth:"+provider+":user_info"] = userEnc
	} else {
		delete(creds, "oauth:"+provider+":user_info")
	}
	creds["oauth:active_provider"] = providerEnc
	if err := atomicWriteJSON(f.credentials, creds); err != nil {
		return fmt.Errorf("写回 credentials.json 失败: %w", err)
	}

	// 3. 更新 config.json provider
	providers, _ := cfg["provider"].(map[string]interface{})
	if providers == nil {
		providers = map[string]interface{}{}
		cfg["provider"] = providers
	}
	setProviderKey := func(id, key string, enable bool) {
		p, _ := providers[id].(map[string]interface{})
		if p == nil {
			p = map[string]interface{}{}
			providers[id] = p
		}
		opts, _ := p["options"].(map[string]interface{})
		if opts == nil {
			opts = map[string]interface{}{}
			p["options"] = opts
		}
		opts["apiKey"] = key
		p["enabled"] = enable
	}
	setProviderKey("builtin:"+provider+"-start-plan", a.ZCodeJWT, true)
	setProviderKey("builtin:"+provider+"-coding-plan", a.APIKey, a.APIKey != "")
	if err := atomicWriteJSON(f.config, cfg); err != nil {
		return fmt.Errorf("写回 config.json 失败: %w", err)
	}
	// config.json 现含账号 JWT / API Key，限权 0600
	os.Chmod(f.config, 0600)

	// 4. 清缓存强制重新探测
	os.Remove(f.cache)

	log.Printf("[switch-back] account %s written to local client", a.Email)

	// 5. 可选：结束 ZCode 进程让改动生效
	if killClient {
		killZCodeProcess()
	}
	return nil
}

// atomicWriteJSON 临时文件 + rename 原子写（目标含账号凭证，权限收紧为 0600）
func atomicWriteJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp-zproxy"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

// killZCodeProcess 结束本机 ZCode 客户端进程（平台实现见 proc_windows.go / proc_other.go）
func killZCodeProcess() {
	out, err := killProcessByName("ZCode.exe")
	if err != nil {
		log.Printf("[switch-back] kill ZCode.exe: %v (%s)", err, strings.TrimSpace(out))
		return
	}
	log.Printf("[switch-back] ZCode.exe terminated")
}

// exeDir 返回可执行文件所在目录（备份/数据路径基准）
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// checkSnapshotDecryptable 预检单个快照文件内容：进入 JSON 内部逐值验证 enc 密文
// 在目标 home 密钥下可解，任何失败整体拒绝。整串 enc 前缀判定对快照永远不命中
// （快照值是 JSON 文档），必须深入 JSON 查值。内层 JSON 解析失败同样拒绝——
// 解析不了的快照无从确认密文归属，不是安全的覆盖来源。明文值（无 enc: 前缀）合法。
func checkSnapshotDecryptable(name, content, secret string) error {
	check := func(field, v string) error {
		if !strings.HasPrefix(v, "enc:") {
			return nil
		}
		if !IsEncryptedValue(v) {
			// enc: 前缀但非 enc:v1 格式：本地客户端同样解不开，一并拒绝
			return fmt.Errorf("快照 %s 的 %s 是无法识别的加密格式（可能来自多实例实例目录），拒绝还原以免登出本地客户端", name, field)
		}
		if _, err := DecryptCredential(v, secret); err != nil {
			return fmt.Errorf("快照 %s 的 %s 无法在本地客户端密钥下解密（可能来自多实例实例目录），拒绝还原以免登出本地客户端: %w", name, field, err)
		}
		return nil
	}
	var doc interface{}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return fmt.Errorf("快照 %s 内层 JSON 解析失败，拒绝还原（解析不了的快照不安全覆盖现网文件）: %w", name, err)
	}
	switch name {
	case "credentials.json":
		// 递归下探每个字符串值：形状比导入的 map[string]string 更宽也照查，
		// 密文藏在任何嵌套层都不能漏
		var walkErr error
		var walk func(path string, v interface{})
		walk = func(path string, v interface{}) {
			if walkErr != nil {
				return
			}
			switch t := v.(type) {
			case map[string]interface{}:
				for k, vv := range t {
					kpath := k
					if path != "" {
						kpath = path + "." + k
					}
					walk(kpath, vv)
				}
			case []interface{}:
				for i, vv := range t {
					walk(fmt.Sprintf("%s[%d]", path, i), vv)
				}
			case string:
				walkErr = check(path, t)
			}
		}
		walk("", doc)
		return walkErr
	case "config.json":
		// 与导入路径（importLocalClient 提取 apiKey）同形遍历 provider.options.apiKey
		var cfg struct {
			Provider map[string]struct {
				Options struct {
					APIKey string `json:"apiKey"`
				} `json:"options"`
			} `json:"provider"`
		}
		if err := json.Unmarshal([]byte(content), &cfg); err != nil {
			return fmt.Errorf("快照 %s 内层 JSON 解析失败，拒绝还原（解析不了的快照不安全覆盖现网文件）: %w", name, err)
		}
		for id, p := range cfg.Provider {
			if err := check(id+"/options.apiKey", p.Options.APIKey); err != nil {
				return err
			}
		}
	}
	return nil
}

// RestoreLocalFromSnapshot 用导入时的快照还原本地客户端（撤销切回）。
// 与切回同一不可逆纪律：先备份现网凭证再动笔；多实例快照以实例目录推导密钥，
// 而还原目标恒为主 home——先深入快照 JSON 内部、按主 home 密钥预检每个 enc 密文值
// 可解密，避免把别的实例目录加密的快照盖到主目录后客户端全体解密失败、登录态尽失。
func (m *AccountManager) RestoreLocalFromSnapshot(accountID int64) error {
	a, err := m.db.GetAccount(accountID)
	if err != nil {
		return err
	}
	if a.CredsRaw == "" {
		return fmt.Errorf("该账号没有本地凭证快照，无法还原")
	}
	var snapshot map[string]string
	if err := json.Unmarshal([]byte(a.CredsRaw), &snapshot); err != nil {
		return fmt.Errorf("快照解析失败: %w", err)
	}
	f := resolveLocalClientFiles()
	secret := DefaultCredentialSecret(f.home)
	// 预检：进入快照 JSON 内部逐个密文值验证目标 home 密钥可解，全部通过才动笔。
	// 整串 enc:v1 前缀判定对快照永远不命中（快照值是 { 开头的 JSON 文档），
	// 旧写法等同没有预检；只扫 targets 白名单内的文件，与写入范围一致
	for _, name := range []string{"credentials.json", "config.json"} {
		content, ok := snapshot[name]
		if !ok || content == "" {
			continue
		}
		if err := checkSnapshotDecryptable(name, content, secret); err != nil {
			return err
		}
	}
	// 备份现网凭证：还原是覆盖性写入，备份是唯一可逆手段（与切回一致）
	backupDir := filepath.Join(exeDir(), "data", "backups")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return fmt.Errorf("创建备份目录失败: %w", err)
	}
	stamp := time.Now().Format("20060102-150405")
	for _, p := range []string{f.credentials, f.config} {
		if data, err := os.ReadFile(p); err == nil {
			bak := filepath.Join(backupDir, filepath.Base(p)+".restore."+stamp+".bak")
			if err := os.WriteFile(bak, data, 0600); err != nil {
				return fmt.Errorf("备份 %s 失败: %w", filepath.Base(p), err)
			}
			if err := os.Chmod(bak, 0600); err != nil {
				return fmt.Errorf("收紧备份权限失败: %w", err)
			}
		}
	}
	targets := map[string]string{
		"credentials.json": f.credentials,
		"config.json":      f.config,
	}
	for name, content := range snapshot {
		path, ok := targets[name]
		if !ok || content == "" {
			continue
		}
		tmp := path + ".tmp-zproxy"
		// 快照含账号 JWT / API Key，与切回路径同样限权 0600；
		// 临时文件复用旧名时 WriteFile 不改既有权限，故显式 Chmod
		if err := os.WriteFile(tmp, []byte(content), 0600); err != nil {
			return err
		}
		if err := os.Chmod(tmp, 0600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	if _, hasCreds := snapshot["credentials.json"]; hasCreds {
		if _, hasCfg := snapshot["config.json"]; !hasCfg {
			// 快照缺 config.json（导入时不可读）：切回写入的 provider keys
			// 会与快照登录态混装，如实告警而不是静默半还原
			log.Printf("[switch-back] WARNING: snapshot of account %s has credentials.json but no config.json; provider keys written by switch-back remain in config.json", a.Email)
		}
	}
	os.Remove(f.cache)
	log.Printf("[switch-back] local client restored from snapshot of account %s", a.Email)
	return nil
}
