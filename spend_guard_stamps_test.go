package main

// 冻结测试（2026-10-01 评审修复波）：锁定评审发现 F3 的期望行为——防双花闸门
// 必须把"无归属盖印"（lastExpirySpendSlot==0，手动/阈值路径消耗留下的印记）
// 保守地视为拒绝任何临期消耗。修复方不得修改本文件。
//
// 契约（spend_guard_stamps_test.go + 修复方共同遵守）：
//  1. 每一次成功的 UseReset 都必须盖印（时刻 + 槽位 expireAt；调用方不知道
//     expireAt 时盖 0 = 无归属）。
//  2. 闸门 gap 谓词：time.Since(last) < gap 且 (lastSlot == 0 或 expireAt ==
//     lastSlot) → 拒绝；否则放行。无归属印记保守地挡住一切（代价：手动/阈值
//     消耗后 ≤gap 内另一个槽位的临期消耗会被暂缓——双花方向的安全取舍）。
//
// 覆盖说明：本文件锁谓词（闸门侧）；"手动/阈值路径实际盖印"由修复方的
// spend_guard_fix_test.go 负责（需要上游 stub 接缝）。

import (
	"errors"
	"testing"
	"time"
)

// TestGuardRefusesUnattributedStampWithinGap 无归属盖印（slot=0，模拟手动/
// 阈值消耗后应留下的印记）在 gap 内必须拒绝任何槽位的临期消耗。
// ca47015 的谓词只比对 expireAt == lastSlot，0 ≠ 500 → 放行 → 双花（评审 F3）。
func TestGuardRefusesUnattributedStampWithinGap(t *testing.T) {
	const id = int64(990201)
	clean := func() {
		autoResetState.Lock()
		delete(autoResetState.lastExpirySpend, id)
		delete(autoResetState.lastExpirySpendSlot, id)
		autoResetState.Unlock()
	}
	clean()
	defer clean()

	// 模拟手动/阈值路径成功消耗后应留下的无归属盖印（修复方负责让真实路径盖出它）
	autoResetState.Lock()
	autoResetState.lastExpirySpend[id] = time.Now()
	autoResetState.lastExpirySpendSlot[id] = 0
	autoResetState.Unlock()

	calls := 0
	spent, reason := spendExpiringSlotGuarded(id, 500, func() error { calls++; return nil })
	if spent || reason != spendGuardGapRefused {
		t.Fatalf("unattributed stamp within gap: spent=%v reason=%q, want false/%s", spent, reason, spendGuardGapRefused)
	}
	if calls != 0 {
		t.Fatalf("refused spend must not call spend fn, calls=%d", calls)
	}
}

// TestGuardUnattributedStampExpiresAfterGap 无归属盖印过 gap 后放行（只挡
// 窗口内，不做永久熔断）。
func TestGuardUnattributedStampExpiresAfterGap(t *testing.T) {
	const id = int64(990202)
	clean := func() {
		autoResetState.Lock()
		delete(autoResetState.lastExpirySpend, id)
		delete(autoResetState.lastExpirySpendSlot, id)
		autoResetState.Unlock()
	}
	clean()
	defer clean()

	autoResetState.Lock()
	autoResetState.lastExpirySpend[id] = time.Now().Add(-autoResetExpirySpendGap - time.Minute)
	autoResetState.lastExpirySpendSlot[id] = 0
	autoResetState.Unlock()

	calls := 0
	spent, reason := spendExpiringSlotGuarded(id, 500, func() error { calls++; return nil })
	if !spent || reason != "" || calls != 1 {
		t.Fatalf("post-gap unattributed: spent=%v reason=%q calls=%d, want true/\"\"/1", spent, reason, calls)
	}
}

// TestGuardDifferentSlotStillAllowed 无回归：双方都归属明确（已知 expireAt）
// 时，gap 内不同槽位照旧放行（锁内重拉列表即上游亲证是另一槽）。
func TestGuardDifferentSlotStillAllowed(t *testing.T) {
	const id = int64(990203)
	clean := func() {
		autoResetState.Lock()
		delete(autoResetState.lastExpirySpend, id)
		delete(autoResetState.lastExpirySpendSlot, id)
		autoResetState.Unlock()
	}
	clean()
	defer clean()

	calls := 0
	okSpend := func() error { calls++; return nil }

	if spent, reason := spendExpiringSlotGuarded(id, 1000, okSpend); !spent || reason != "" {
		t.Fatalf("first attributed spend: spent=%v reason=%q, want true/\"\"", spent, reason)
	}
	if spent, reason := spendExpiringSlotGuarded(id, 2000, okSpend); !spent || reason != "" {
		t.Fatalf("gap different-slot must stay allowed: spent=%v reason=%q", spent, reason)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

// TestGuardFailedSpendLeavesNoStamp 无回归：动笔失败不盖印（下轮可重试）。
func TestGuardFailedSpendLeavesNoStamp(t *testing.T) {
	const id = int64(990204)
	clean := func() {
		autoResetState.Lock()
		delete(autoResetState.lastExpirySpend, id)
		delete(autoResetState.lastExpirySpendSlot, id)
		autoResetState.Unlock()
	}
	clean()
	defer clean()

	spent, reason := spendExpiringSlotGuarded(id, 700, func() error { return errors.New("boom") })
	if spent || reason != "boom" {
		t.Fatalf("failed spend: spent=%v reason=%q, want false/boom", spent, reason)
	}
	autoResetState.Lock()
	_, stamped := autoResetState.lastExpirySpend[id]
	autoResetState.Unlock()
	if stamped {
		t.Fatal("failed spend must not stamp")
	}
}
