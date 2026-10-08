package main

import (
	"testing"
	"time"
)

// R4 风控阶梯冷却：24h 窗口内 1 次→120s、2 次→30min、3+ 次→24h
func TestMarkRiskCoolingLadder(t *testing.T) {
	p, _ := newPoolTestPool(t)
	a := mkPoolAccount("risk", DefaultPriority, StatusActive)

	dur := func(a *Account) time.Duration {
		p.MarkRiskCooling(a, "上游风控拦截（unusual activity），全通道失败")
		p.mu.Lock()
		defer p.mu.Unlock()
		return time.Duration(a.CoolingUntil-time.Now().Unix()) * time.Second
	}

	if got := dur(a); got > 125*time.Second || got < 115*time.Second {
		t.Errorf("1st strike: want ~120s, got %v", got)
	}
	if got := dur(a); got < 29*time.Minute {
		t.Errorf("2nd strike: want ~30min, got %v", got)
	}
	if got := dur(a); got < 23*time.Hour {
		t.Errorf("3rd strike: want ~24h, got %v", got)
	}
	if got := dur(a); got < 23*time.Hour {
		t.Errorf("4th strike: want capped ~24h, got %v", got)
	}
	if n := p.riskStrikeCount(a.ID); n != 4 {
		t.Errorf("riskStrikeCount = %d, want 4", n)
	}
	if a.Status != StatusCooling {
		t.Errorf("status = %s, want cooling", a.Status)
	}
}

// 窗口外的拦截不累积：清掉窗口（模拟 24h 前的记录自然过期）后回到第 1 级
func TestRiskStrikesExpire(t *testing.T) {
	p, _ := newPoolTestPool(t)
	a := mkPoolAccount("risk-exp", DefaultPriority, StatusActive)

	p.MarkRiskCooling(a, "strike-1")
	p.MarkRiskCooling(a, "strike-2")
	if p.riskStrikeCount(a.ID) != 2 {
		t.Fatal("expected 2 strikes")
	}
	// 手动老化：全部移到窗口外
	p.mu.Lock()
	old := time.Now().Add(-25 * time.Hour)
	p.riskStrikes[a.ID] = []time.Time{old, old}
	p.mu.Unlock()

	dur := func(a *Account) time.Duration {
		p.MarkRiskCooling(a, "strike-after-expiry")
		p.mu.Lock()
		defer p.mu.Unlock()
		return time.Duration(a.CoolingUntil-time.Now().Unix()) * time.Second
	}
	if got := dur(a); got > 125*time.Second || got < 115*time.Second {
		t.Errorf("expired strikes must not escalate: want ~120s, got %v", got)
	}
}
