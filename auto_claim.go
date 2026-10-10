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
	autoClaimDefaultIntervalMin = 30   // 每轮间隔（分钟）
	autoClaimDefaultDelaySec    = 10   // 账号间防风控延迟（秒）
	autoClaimMinIntervalMin     = 5    // 间隔下限：过于频繁的批量 detect/claim 是风控信号
	autoClaimMaxIntervalMin     = 1440 // 间隔上限：超大值经 time.Duration(mins)*time.Minute 溢出为负 → 热循环
)

type AutoClaimer struct {
	db   *DB
	zapi *ZCodeAPI

	mu          sync.Mutex
	clampLogged string // 已告警过的超限原始值（钳制告警去重，不刷屏）

	stopCh   chan struct{}
	stopOnce sync.Once
	loopDone chan struct{} // 循环 goroutine 退出标记（Stop 有界等待）
}

func NewAutoClaimer(db *DB, zapi *ZCodeAPI) *AutoClaimer {
	return &AutoClaimer{db: db, zapi: zapi, stopCh: make(chan struct{}), loopDone: make(chan struct{})}
}

// Stop 停止自动领取（幂等）：关停信号 + 有界等循环退出——claim 成功后的
// 账本写库必须先于 main 的 db.Close，否则领了活动却丢记录
func (ac *AutoClaimer) Stop() {
	ac.stopOnce.Do(func() { close(ac.stopCh) })
	if ac.loopDone != nil {
		select {
		case <-ac.loopDone:
		case <-time.After(30 * time.Second):
			log.Printf("[auto-claim] stop: round still running after 30s")
		}
	}
}

// Start 启动自动领取循环
func (ac *AutoClaimer) Start() {
	go func() {
		defer close(ac.loopDone)
		// 首轮延迟 90s：等服务与额度刷新稳定，避免启动风暴与上游调用叠加
		t := time.NewTimer(90 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ac.stopCh:
				return
			case <-t.C:
			}
			iv := ac.interval()
			if iv > 0 && ac.enabled() {
				// 常驻循环：单轮 panic 不得带走整个 goroutine（net/http 管不到这里）
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[auto-claim] round panic: %v", r)
						}
					}()
					ac.RunOnce("cron")
				}()
			}
			if iv == 0 {
				// 手动模式（间隔显式 0）：循环空转，每分钟复查设置是否改回
				iv = time.Minute
			}
			t.Reset(iv)
		}
	}()
	log.Printf("[auto-claim] started")
}

func (ac *AutoClaimer) enabled() bool {
	v, _ := ac.db.GetSetting("auto_claim_promos")
	return v != "0" && v != "false"
}

// interval 轮询间隔（分钟）：钳制 [5,1440]；非法/负数回落默认 30；
// 显式 0 = 手动模式：循环不自动执行，RunOnce 留给手动触发
func (ac *AutoClaimer) interval() time.Duration {
	mins := autoClaimDefaultIntervalMin
	if v, _ := ac.db.GetSetting("auto_claim_interval_minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n == 0 {
				return 0
			}
			if n > autoClaimMaxIntervalMin {
				// 超上限：钳住（超大分钟数溢出 Duration 为负，t.Reset(负值)
				// 立即触发 → 循环空转热循环）
				ac.noteIntervalClamp(v)
				n = autoClaimMaxIntervalMin
			}
			mins = n
		}
	}
	if mins < 0 {
		mins = autoClaimDefaultIntervalMin
	}
	if mins < autoClaimMinIntervalMin {
		mins = autoClaimMinIntervalMin
	}
	// +0~40% 抖动（只延长不缩短，风控保守方向）：固定周期批量打 detect 接口本身就是可聚类特征
	jitter := time.Duration(rand.Intn(int(float64(mins)*0.4))) * time.Minute
	return time.Duration(mins)*time.Minute + jitter
}

// noteIntervalClamp 超限钳制告警：同一原始值只告警一次（循环每轮读一次设置）
func (ac *AutoClaimer) noteIntervalClamp(v string) {
	ac.mu.Lock()
	first := ac.clampLogged != v
	ac.clampLogged = v
	ac.mu.Unlock()
	if first {
		log.Printf("[auto-claim] auto_claim_interval_minutes=%s 超过上限 %d 分钟，钳制为 %d", v, autoClaimMaxIntervalMin, autoClaimMaxIntervalMin)
	}
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
		// 停机感知：claim 的上游消耗不可撤销，账本写库依赖库仍开着
		select {
		case <-ac.stopCh:
			log.Printf("[auto-claim] (%s) aborted by shutdown before %s", trigger, a.DisplayNameOrEmail())
			return
		default:
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
