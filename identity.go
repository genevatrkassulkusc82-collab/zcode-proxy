package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ---- 账号设备身份（R5）：per-account device_mid 回填 + 遥测/安装仿真 ----
// OAuth/粘贴导入的账号 DeviceMid 常为空，多账号共享网关侧设备信号；
// 此处按账号补齐持久化身份，并按官方客户端首启顺序上报激活事件
// （端点/事件体与 dengyie/zcode2api app/install.py、app/telemetry.py 逐字段对齐）。

// SetAccountDeviceMid 回填账号设备指纹（不覆盖已有值由调用方保证：仅空值时调用）
func (db *DB) SetAccountDeviceMid(id int64, deviceMid string) error {
	_, err := db.conn.Exec(`UPDATE accounts SET device_mid = ?,
		updated_at = datetime('now','localtime') WHERE id = ?`, deviceMid, id)
	return err
}

// EnsureDeviceIdentity 返回账号设备指纹；为空时生成 UUIDv4 并落库。
// 每个请求 goroutine 持有独立的 Account 副本（database_accounts.go），
// a.mu 只防单副本内并发；跨副本极端情况下可能各生成一次，落库后写覆盖
// 为良性竞争（与既有"最后写赢"字段语义一致）。落库失败时清空内存值，
// 下次调用重试，避免内存与 DB 永久漂移。
func (db *DB) EnsureDeviceIdentity(a *Account) string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	generated := a.DeviceMid == ""
	if generated {
		a.DeviceMid = uuid.NewString()
	}
	mid := a.DeviceMid
	a.mu.Unlock()
	if !generated {
		return mid
	}
	if err := db.SetAccountDeviceMid(a.ID, mid); err != nil {
		log.Printf("[identity] account %d persist device_mid: %v", a.ID, err)
		a.mu.Lock()
		a.DeviceMid = ""
		a.mu.Unlock()
		return ""
	}
	log.Printf("[identity] account %d device identity generated", a.ID)
	return mid
}

// EnsurePoolDeviceIdentities 启动期全量回填空 device_mid 账号；返回回填数量
func EnsurePoolDeviceIdentities(db *DB) int {
	accounts, err := db.ListAccounts("")
	if err != nil {
		log.Printf("[identity] pool backfill list accounts: %v", err)
		return 0
	}
	backfilled := 0
	for _, a := range accounts {
		if a.DeviceMid == "" {
			if db.EnsureDeviceIdentity(a) != "" {
				backfilled++
			}
		}
	}
	if backfilled > 0 {
		log.Printf("[identity] pool backfill: %d/%d accounts got device identity", backfilled, len(accounts))
	}
	return backfilled
}

// ---- 桌面 SKU 指纹池（遥测负载用） ----

// DesktopFingerprint 单台"设备"的桌面形态描述
type DesktopFingerprint struct {
	OS          string // os_category 口径：darwin | windows
	OSVersion   string
	DeviceModel string
}

// randomDesktopFingerprint 加权抽取桌面 SKU：macOS Sequoia 45%（Mac15,6/Mac14,8）、
// macOS Tahoe 25%、Windows 11 30%（Surface_Laptop_Studio_2/PC）
func randomDesktopFingerprint() DesktopFingerprint {
	switch r := rand.IntN(100); {
	case r < 25:
		return DesktopFingerprint{OS: "darwin", OSVersion: "15.6.0", DeviceModel: "Mac15,6"}
	case r < 45:
		return DesktopFingerprint{OS: "darwin", OSVersion: "15.6.0", DeviceModel: "Mac14,8"}
	case r < 70:
		return DesktopFingerprint{OS: "darwin", OSVersion: "26.0.0", DeviceModel: "Mac16,1"}
	default:
		if rand.IntN(2) == 0 {
			return DesktopFingerprint{OS: "windows", OSVersion: "10.0.26100", DeviceModel: "Surface_Laptop_Studio_2"}
		}
		return DesktopFingerprint{OS: "windows", OSVersion: "10.0.26100", DeviceModel: "PC"}
	}
}

// ---- 安装序仿真（install.py run_install_sequence_for_account 移植） ----

// installIDs 账号安装身份（仅内存，不落库：无 schema 变更）
var installIDs sync.Map // account id (int64) -> install_id (string)

func installIDFor(accountID int64) string {
	if v, ok := installIDs.Load(accountID); ok {
		return v.(string)
	}
	id := uuid.NewString()
	actual, _ := installIDs.LoadOrStore(accountID, id)
	return actual.(string)
}

// activationElements 官方首启事件序（install.py：app_launch → app_daily_active，
// 日活去重由上游按 device_mid+日期计算，同日重复上报无副作用）
var activationElements = [...]string{"app_launch", "app_daily_active"}

// SendInstallSequence 按官方客户端首启顺序请求一遍：
//  1. GET /api/v1/client/configs?app_version=…（免鉴权；带 platform 参数会被 3001 拒绝）
//  2. POST /api/v1/event/report ×2（app_launch + app_daily_active）
//
// 端点/事件体字段集与 zcode2api install.py + telemetry.py 一致（EventReportURL /
// ClientConfigsURL 与参考实现同源）。任何失败仅记录日志，绝不影响调用方。
func (z *ZCodeAPI) SendInstallSequence(a *Account, httpClient *http.Client) {
	if a == nil || a.ID <= 0 {
		return
	}
	if httpClient == nil {
		httpClient = ClientForURL(z.egress.ProxyURLForAccount(a), ClientConfigsURL, 5*time.Second)
	}
	fp := randomDesktopFingerprint()
	installID := installIDFor(a.ID)
	userID := z.telemetryUserID(a)

	do := func(method, urlStr string, payload interface{}, withAuth bool) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var body io.Reader
		if payload != nil {
			buf, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			body = bytes.NewReader(buf)
		}
		req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
		if err != nil {
			return err
		}
		id := NewClientIdentity(z.appVersion, a.DeviceMid)
		for k, v := range ZaiClientHeaders(id) {
			req.Header.Set(k, v)
		}
		if withAuth {
			if token := z.billingToken(a); token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 120))
		}
		var v map[string]interface{}
		json.Unmarshal(raw, &v)
		if code := jsonInt(v, "code"); code != 0 && code != -1 && code != 200 {
			return fmt.Errorf("业务码 %d: %s", code, truncate(string(raw), 120))
		}
		return nil
	}

	// 1) client/configs：免鉴权端点，仅 app_version 查询参数
	if err := do(http.MethodGet,
		ClientConfigsURL+"?app_version="+url.QueryEscape(z.appVersion),
		nil, false); err != nil {
		log.Printf("[install] account %d client/configs: %v", a.ID, err)
	}

	// 2) event/report：事件体 16 字段与官方 sendReport 逐字段一致
	for _, element := range activationElements {
		payload := map[string]interface{}{
			"event_id":           uuid.NewString(),
			"client_timezone":    clientTimezoneValue(),
			"client_language":    zcodeLang,
			"element_name":       element,
			"event_region":       "app",
			"event_type":         "view",
			"event_text":         "",
			"event_extra_detail": map[string]interface{}{},
			"user_id":            userID,
			"screen_resolution":  screenResolution,
			"app_version":        z.appVersion,
			"device_os_category": fp.OS,
			"device_os_version":  fp.OSVersion,
			"device_mid":         a.DeviceMid,
			"mac_id":             "",
			"marketing_params":   "{}",
		}
		if err := do(http.MethodPost, EventReportURL, payload, true); err != nil {
			log.Printf("[install] account %d event/report %s: %v", a.ID, element, err)
		}
	}
	log.Printf("[install] account %d install sequence done (install_id=%s, sku=%s/%s/%s)",
		a.ID, installID, fp.OS, fp.OSVersion, fp.DeviceModel)
}

// ensureAccountIdentity 补齐设备身份并异步仿真一次官方安装序（仅 JWT 账号；
// API Key 账号无 zcode.z.ai 登录态，保持未登录安装形态不发）。导入路径共用。
func (m *AccountManager) ensureAccountIdentity(a *Account) {
	if a == nil || a.ID <= 0 {
		return
	}
	m.db.EnsureDeviceIdentity(a)
	if !strings.HasPrefix(a.AuthType, "jwt") {
		return
	}
	go func(acc *Account) {
		client := ClientForURL(m.zapi.egress.ProxyURLForAccount(acc), EventReportURL, 5*time.Second)
		m.zapi.SendInstallSequence(acc, client)
	}(a)
}
