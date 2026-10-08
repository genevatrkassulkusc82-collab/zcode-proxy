package main

import (
	"testing"
	"time"
)

// TestMatchFieldTruthTable 锁死 cron 匹配器的关键性质：
// 校验接受的表达式必须在字段范围内至少命中一个值（"通过校验却永不触发"
// 正是历史上 */3,15 缺陷的类别），并钉住代表性表达式的精确命中值。
func TestMatchFieldTruthTable(t *testing.T) {
	ranges := map[string][2]int{
		"minute": {0, 59},
		"hour":   {0, 23},
		"dom":    {1, 31},
		"month":  {1, 12},
		"dow":    {0, 6},
	}
	cases := []struct {
		expr      string
		field     string
		accepted  bool // validateCronField 是否接受
		wantMatch map[int]bool
	}{
		// 基础形态
		{expr: "*", field: "minute", accepted: true},
		{expr: "5", field: "minute", accepted: true, wantMatch: map[int]bool{5: true, 6: false}},
		{expr: "60", field: "minute", accepted: false},
		{expr: "0", field: "dow", accepted: true, wantMatch: map[int]bool{0: true, 1: false}},
		{expr: "7", field: "dow", accepted: false}, // 比标准 cron 严格（7=周日），保存时报错
		// 步进
		{expr: "*/3", field: "minute", accepted: true, wantMatch: map[int]bool{0: true, 1: false, 3: true, 58: false, 57: true}},
		{expr: "*/0", field: "minute", accepted: false},
		{expr: "*/-3", field: "minute", accepted: false},
		{expr: "*/x", field: "minute", accepted: false},
		{expr: "*/3,15", field: "hour", accepted: true, wantMatch: map[int]bool{0: true, 1: false, 3: true, 15: true, 16: false}}, // 回归：*/3,15 曾永不触发
		{expr: "9,*/3", field: "hour", accepted: true, wantMatch: map[int]bool{9: true, 6: true, 7: false}},
		{expr: "1-3,*/2", field: "dom", accepted: true},
		// 范围与 n/m
		{expr: "1-5", field: "minute", accepted: true, wantMatch: map[int]bool{1: true, 5: true, 6: false, 0: false}},
		{expr: "5-1", field: "minute", accepted: false},
		{expr: "1-70", field: "minute", accepted: false}, // 70 超出分钟域（1-40 本身合法）
		{expr: "5-", field: "minute", accepted: false},
		{expr: "-5", field: "minute", accepted: false},
		{expr: "1-2-3", field: "minute", accepted: false},
		{expr: "1-5/2", field: "minute", accepted: true, wantMatch: map[int]bool{1: true, 3: true, 5: true, 2: false}},
		{expr: "1-5/0", field: "minute", accepted: false},
		{expr: "5/10", field: "minute", accepted: true, wantMatch: map[int]bool{5: true, 15: true, 10: false, 25: true}},
		{expr: "0/5", field: "minute", accepted: true},
		{expr: "*/3/5", field: "minute", accepted: false},
		{expr: "1,,2", field: "minute", accepted: false},
		{expr: "1,1,2", field: "minute", accepted: true, wantMatch: map[int]bool{1: true, 2: true, 3: false}},
		{expr: "9,*", field: "minute", accepted: true},
		{expr: "*,9", field: "minute", accepted: true},
	}
	for _, tc := range cases {
		mn, mx := ranges[tc.field][0], ranges[tc.field][1]
		err := validateCronField(tc.expr, mn, mx)
		if tc.accepted && err != nil {
			t.Errorf("validate %q: expected accept, got %v", tc.expr, err)
			continue
		}
		if !tc.accepted {
			if err == nil {
				t.Errorf("validate %q: expected reject, got accept", tc.expr)
			}
			continue
		}
		// 性质：接受的表达式必须至少命中一个值（静默死计划缺陷类别）
		any := false
		for v := mn; v <= mx; v++ {
			if matchField(tc.expr, v, mn, mx) {
				any = true
				break
			}
		}
		if !any {
			t.Errorf("match %q (%s): accepted but never fires — silent dead expression", tc.expr, tc.field)
			continue
		}
		for v, want := range tc.wantMatch {
			if got := matchField(tc.expr, v, mn, mx); got != want {
				t.Errorf("match %q (%s) at %d: got %v want %v", tc.expr, tc.field, v, got, want)
			}
		}
	}
}

// TestShouldRunDomDow 钉住 日/周 的 AND/OR 组合语义
func TestShouldRunDomDow(t *testing.T) {
	cases := []struct {
		expr string
		day  int // 2026-01 的某日（1=周四，3=周六，4=周日，5=周一）
		want bool
	}{
		{expr: "0 0 1 * 1", day: 1, want: true},    // 1号 或 周一（OR：两受限字段）
		{expr: "0 0 1 * 1", day: 5, want: true},    // 2026-01-05 是周一 → OR 命中
		{expr: "0 0 1 * 1", day: 2, want: false},   // 非 1 号且非周一
		{expr: "0 0 */2 * 1", day: 3, want: false}, // 奇数日且周一（*/2 算星号位 → AND）；3号是周六
		{expr: "0 0 */2 * 1", day: 5, want: true},  // 5号是周一且为奇数日 → AND 满足
		{expr: "0 0 */2 * 1", day: 4, want: false}, // 4号是周日且偶数日 → AND 不满足
		{expr: "0 0 1 * *", day: 2, want: false},   // 仅 1 号
		{expr: "0 0 * * 1", day: 5, want: true},    // 仅周一
	}
	for _, tc := range cases {
		now := time.Date(2026, 1, tc.day, 0, 0, 0, 0, time.Local)
		if got := shouldRun(tc.expr, now); got != tc.want {
			t.Errorf("shouldRun %q on 2026-01-%02d: got %v want %v", tc.expr, tc.day, got, tc.want)
		}
	}
}
