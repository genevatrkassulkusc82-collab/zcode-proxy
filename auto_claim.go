package main

import (
	"log"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- 自动领取促销活动 ----
// 常驻循环：检测各账号可用活动并自动领取优先级最高的（复用 ClaimForAccount：
// detect → pick → captcha → claim → claim_records，含账号级互斥）。
// 明确边界：只领活动，绝不触碰额度重置——reset 仅支持面板手动触发。

const (
	autoClaimDefaultIntervalMin = 30 // 每轮间隔（分钟）
	autoClaimDefaultDelaySec    = 10 // 账号间防风控延迟（秒）
	autoClaimMinIntervalMin     = 5  // 间隔下限：过于频繁的批量 detect/claim 是风控信号
)

type AutoClaimer struct {
	db   *DB
	zapi *ZCodeAPI

	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewAutoClaimer(db *DB, zapi *ZCodeAPI) *AutoClaimer {
	return &AutoClaimer{db: db, zapi: zapi, stopCh: make(chan struct{})}
}

func (ac *AutoClaimer) Stop() {
	ac.stopOnce.Do(func() { close(ac.stopCh) })
}

// Start 启动自动领取循环
func (ac *AutoClaimer) Start() {
	go func() {
		// 首轮延迟 90s：等服务与额度刷新稳定，避免启动风暴与上游调用叠加
		t := time.NewTimer(90 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ac.stopCh:
				return
			case <-t.C:
			}
			if ac.enabled() {
				ac.RunOnce("cron")
			}
			t.Reset(ac.interval())
		}
	}()
	log.Printf("[auto-claim] started")
}

func (ac *AutoClaimer) enabled() bool {
	v, _ := ac.db.GetSetting("auto_claim_promos")
	return v != "0" && v != "false"
}

// interval 轮询间隔（分钟）：设置非法回落默认；显式 0 = 关闭循环节奏外的
// 额外执行（循环仍空转等待，RunOnce 可由手动 API 触发）
func (ac *AutoClaimer) interval() time.Duration {
	mins := autoClaimDefaultIntervalMin
	if v, _ := ac.db.GetSetting("auto_claim_interval_minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			mins = n
		}
	}
	if mins <= 0 {
		mins = autoClaimDefaultIntervalMin
	}
	if mins < autoClaimMinIntervalMin {
		mins = autoClaimMinIntervalMin
	}
	// ±20% 抖动：固定周期批量打 detect 接口本身就是可聚类特征
	jitter := time.Duration(rand.Intn(int(float64(mins) * 0.4))) * time.Minute
	return time.Duration(mins)*time.Minute + jitter
}

func (ac *AutoClaimer) delaySeconds() int {
	if v, _ := ac.db.GetSetting("auto_claim_delay_seconds"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return autoClaimDefaultDelaySec
}

// RunOnce 执行一轮：全部可领账号依序 detect+claim 最高优先级活动。
// 供循环与手动触发共用；账号级互斥保证与面板/计划并发安全。
func (ac *AutoClaimer) RunOnce(trigger string) {
	accounts, err := ac.db.ListAccounts("")
	if err != nil {
		log.Printf("[auto-claim] list accounts: %v", err)
		return
	}
	targets := filterRunnableFor("claim", accounts)
	if len(targets) == 0 {
		log.Printf("[auto-claim] (%s) no eligible accounts", trigger)
		return
	}
	delay := ac.delaySeconds()
	log.Printf("[auto-claim] (%s) start, %d account(s), delay=%ds", trigger, len(targets), delay)

	ok, empty := 0, 0
	for i, a := range targets {
		if i > 0 && delay > 0 {
			jitter := rand.Intn(delay/2 + 1)
			select {
			case <-ac.stopCh:
				return
			case <-time.After(time.Duration(delay+jitter) * time.Second):
			}
		}
		result := ac.zapi.ClaimForAccount(a)
		switch {
		case result.OK && strings.Contains(result.Message, "无可领取活动"):
			empty++
		case result.OK:
			ok++
		}
	}
	log.Printf("[auto-claim] (%s) done: claimed=%d nothing-new=%d fail=%d",
		trigger, ok, empty, len(targets)-ok-empty)
}
