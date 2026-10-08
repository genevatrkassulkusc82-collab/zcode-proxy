package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- cancel-spray：客户端断开不得给排队中的健康账号泼假“并发已满”冷却 ----

// 客户端已断开（ctx 取消）时闸门排队失败：立即终止（outcomeUpstreamError），
// 免费/付费两侧账号状态分毫不动。修复前：无条件 Mark*Cooling 10s，一次断连
// 最多给 ~10 个健康账号泼假“并发已满”冷却
func TestForwardOnceClientCancelSkipsCooldown(t *testing.T) {
	p, db := newPoolTestPool(t)
	a := mkDualAccount("cancelspray", StatusActive)
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	a.ID = id
	if err := db.SetSetting("max_concurrent_per_account", "1"); err != nil {
		t.Fatal(err)
	}
	z := &ZCodeAPI{cfg: &FileConfig{}, db: db, pool: p}

	for _, channel := range []string{ChannelFree, ChannelPaid} {
		// 占满唯一名额：闸门排队必然失败
		rel, ok := p.AcquireAccountSlot(context.Background(), a, channel, 50*time.Millisecond)
		if !ok {
			t.Fatalf("%s: holding the only slot should succeed", channel)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
		out := z.forwardOnce(httptest.NewRecorder(), r, a, []byte("{}"), "", "", false, 2,
			&relayCtx{proto: protocolAnthropic}, time.Now(), "test", channel)
		if out != outcomeUpstreamError {
			t.Fatalf("%s: canceled client should end as outcomeUpstreamError, got %v", channel, out)
		}
		fresh, err := db.GetAccount(id)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Status != StatusActive || fresh.CoolingUntil != 0 {
			t.Fatalf("%s: client cancel must not cool the free side, got status=%s until=%d",
				channel, fresh.Status, fresh.CoolingUntil)
		}
		if fresh.PaidCoolingUntil != 0 {
			t.Fatalf("%s: client cancel must not cool the paid side, got %d", channel, fresh.PaidCoolingUntil)
		}
		rel()
	}
}

// ---- 付费限流阶梯：外来冷却时长不得污染升级记忆 ----

// MarkPaidCooling 只把真实阶梯档 {30,120,300} 记入升级记忆；slot 满（10s）、
// 鉴权失败（3600s）、连接失败（60s）等外来值存 0（阶梯不变）。
// 修复前：任意外来值让下一次真实 429 直接跳到 300s
func TestPaidLadderIgnoresForeignCooldowns(t *testing.T) {
	p, db := newPoolTestPool(t)
	a := mkDualAccount("ladder", StatusActive)
	id, err := db.UpsertAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	a.ID = id
	cases := []struct {
		store int
		want  int
	}{
		{10, 30},   // slot 满短冷却：不入阶梯
		{30, 120},  // 真实首档
		{120, 300}, // 真实次档
		{3600, 30}, // 鉴权失败长冷却：不入阶梯
		{60, 30},   // 连接失败默认：不入阶梯
		{300, 300}, // 真实顶档
	}
	for _, c := range cases {
		p.MarkPaidCooling(a, "测试冷却", c.store)
		if got := p.nextPaidCooldown(a); got != c.want {
			t.Fatalf("store %ds → nextPaidCooldown = %d, want %d", c.store, got, c.want)
		}
	}
	p.MarkPaidUsed(a)
	if got := p.nextPaidCooldown(a); got != 30 {
		t.Fatalf("MarkPaidUsed should reset ladder memory, got %d", got)
	}
	_ = id
}

// ---- Anthropic image url-source 块本地拒绝 ----

// 上游只接受 base64 source：url 形态与缺失/null/非对象 source 一律本地校验拒绝
// （修复前 url 形态放行 → 上游 400 + 健康账号白计一次 MarkFailed）
func TestMessagesImageSourceValidation(t *testing.T) {
	build := func(img map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"model": "GLM-4.6",
			"messages": []interface{}{map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "hi"},
					img,
				},
			}},
		}
	}
	cases := []struct {
		name    string
		img     map[string]interface{}
		wantErr bool
	}{
		// url source：现为受控抓取（image_fetch.go），环回地址被 SSRF 防护拒绝
		// （fail-closed，与旧的本地 400 行为等价）
		{"url source", map[string]interface{}{"type": "image",
			"source": map[string]interface{}{"type": "url", "url": "http://127.0.0.1:9/x.png"}}, true},
		{"missing source", map[string]interface{}{"type": "image"}, true},
		{"null source", map[string]interface{}{"type": "image", "source": nil}, true},
		{"non-map source", map[string]interface{}{"type": "image", "source": "https://x/y.png"}, true},
		{"base64 ok", map[string]interface{}{"type": "image",
			"source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": "AAAA"}}, false},
		{"base64 empty data", map[string]interface{}{"type": "image",
			"source": map[string]interface{}{"type": "base64", "data": ""}}, true},
	}
	for _, c := range cases {
		err := validateMessagesBody(context.Background(), build(c.img))
		if c.wantErr && err == nil {
			t.Fatalf("%s: expected validation error, got nil", c.name)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("%s: unexpected error: %v", c.name, err)
		}
		if err != nil && wantImageSourceError(err) == false {
			t.Fatalf("%s: wrong error text: %v", c.name, err)
		}
	}
}

func wantImageSourceError(err error) bool {
	s := err.Error()
	// base64 形状错误（missing/null/非对象/空 data）或 url 抓取失败（SSRF 拦截等）
	return strings.Contains(s, "image block must contain base64 data") ||
		strings.Contains(s, "image url fetch failed")
}

// ---- 间隔设置钳制：极端值不得溢出 Duration 导致热循环 ----

// quota_refresh_interval 读取侧钳制 [30, 86400]s（0=关闭保留）：
// 修复前超大值经 time.Duration(n)*time.Second 溢出为负，time.After(负值)
// 立即触发 → 刷新热循环
func TestQuotaRefreshIntervalClamped(t *testing.T) {
	p, db := newPoolTestPool(t)
	cases := []struct {
		set  string
		want int
	}{
		{"", 60},                      // 未配置：默认
		{"120", 120},                  // 正常值原样
		{"0", 0},                      // 显式 0 = 关闭
		{"-1", 60},                    // 负数：回落默认
		{"5", 30},                     // 低于下限：钳到 30
		{"100000", 86400},             // 超上限：钳到 86400
		{"10000000000", 86400},        // 1e10s：Duration 已溢出为负，必须钳住
		{"999999999999999999", 86400}, // 接近 int64 上限
	}
	for _, c := range cases {
		if err := db.SetSetting("quota_refresh_interval", c.set); err != nil {
			t.Fatal(err)
		}
		if got := p.refreshInterval(); got != c.want {
			t.Fatalf("quota_refresh_interval=%q → refreshInterval() = %d, want %d", c.set, got, c.want)
		}
	}
}

// auto_claim_interval_minutes 读取侧钳制 [5, 1440] 分钟（0=手动模式保留）：
// 修复前超大值 time.Duration(mins)*time.Minute 溢出为负，t.Reset(负值)
// 立即触发 → 循环空转热循环（每轮打 DB + 判定）
func TestAutoClaimIntervalClamped(t *testing.T) {
	_, db := newPoolTestPool(t)
	ac := NewAutoClaimer(db, nil)
	set := func(v string) {
		if err := db.SetSetting("auto_claim_interval_minutes", v); err != nil {
			t.Fatal(err)
		}
	}
	// 超大值：钳到上限（含 0~40% 抖动，只延长不缩短）
	set("999999999")
	if iv := ac.interval(); iv < 1440*time.Minute || iv >= 1440*time.Minute*7/5 {
		t.Fatalf("huge interval should clamp to 1440m (+jitter), got %v", iv)
	}
	// 正常值原样（含抖动）
	set("30")
	if iv := ac.interval(); iv < 30*time.Minute || iv >= 42*time.Minute {
		t.Fatalf("30m should pass through (+jitter), got %v", iv)
	}
	// 低于下限：提升到 5
	set("2")
	if iv := ac.interval(); iv < 5*time.Minute || iv >= 7*time.Minute {
		t.Fatalf("2m should raise to min 5m (+jitter), got %v", iv)
	}
	// 显式 0 = 手动模式保留
	set("0")
	if iv := ac.interval(); iv != 0 {
		t.Fatalf("explicit 0 must stay manual mode, got %v", iv)
	}
}
