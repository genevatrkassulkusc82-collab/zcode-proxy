package main

import (
	"strconv"
	"testing"
	"time"
)

// R: 自动重置阈值策略——"还剩 5/20 分钟自然恢复时绝不动用重置"
func TestAutoResetShouldSpend(t *testing.T) {
	cases := []struct {
		name       string
		waitKnown  bool
		waitSecs   int64
		threshold  int64
		wantSpend  bool
	}{
		{"unknown wait spends", false, 0, 3600, true},
		{"5min left < 60min threshold waits", true, 5 * 60, 60 * 60, false},
		{"20min left < 60min threshold waits", true, 20 * 60, 60 * 60, false},
		{"90min left >= threshold spends", true, 90 * 60, 60 * 60, true},
		{"exactly at threshold spends", true, 60 * 60, 60 * 60, true},
		{"zero threshold always spends", true, 30, 0, true},
		{"already-past window spends", true, 0, 60 * 60, false},
	}
	for _, c := range cases {
		if got := autoResetShouldSpend(c.waitKnown, c.waitSecs, c.threshold); got != c.wantSpend {
			t.Errorf("%s: autoResetShouldSpend(%v,%d,%d) = %v, want %v",
				c.name, c.waitKnown, c.waitSecs, c.threshold, got, c.wantSpend)
		}
	}
}

func TestAutoResetWait(t *testing.T) {
	// 无快照 / 无 next_reset → 未知
	if _, ok := autoResetWait(&Account{}); ok {
		t.Error("empty quota snapshot must report unknown wait")
	}
	if _, ok := autoResetWait(&Account{QuotaJSON: `{"next_reset":0}`}); ok {
		t.Error("zero next_reset must report unknown wait")
	}
	// 未来 30 分钟 → 已知 1800s（±2s 容差）
	future := time.Now().Add(30 * time.Minute).Unix()
	wait, ok := autoResetWait(&Account{QuotaJSON: `{"next_reset":` + strconv.FormatInt(future, 10) + `}`})
	if !ok || wait < 1798 || wait > 1800 {
		t.Errorf("future reset: wait=%d ok=%v, want ~1800/true", wait, ok)
	}
	// 过去的 next_reset → 0（已可自然恢复）
	past := time.Now().Add(-time.Minute).Unix()
	wait, ok = autoResetWait(&Account{QuotaJSON: `{"next_reset":` + strconv.FormatInt(past, 10) + `}`})
	if !ok || wait != 0 {
		t.Errorf("past reset: wait=%d ok=%v, want 0/true", wait, ok)
	}
}
