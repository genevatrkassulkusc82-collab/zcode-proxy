package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- 活动计划调度器 ----
// 5 段 cron（分 时 日 月 周）调度器：分钟级去重 + per-plan 互斥 + 账号间防风控延迟。
// task_type: detect（检测活动）| claim（一键领取）| activate（激活套餐）
// target_type: all_accounts | single_account | group
// 账号间延迟 delay_seconds + 随机抖动（防风控）。

// PlanRunState 计划实时运行状态（前端进度展示）
type PlanRunState struct {
	PlanID         int64  `json:"plan_id"`
	PlanName       string `json:"plan_name"`
	TaskType       string `json:"task_type"`
	Total          int    `json:"total"`
	Done           int    `json:"done"`
	Success        int    `json:"success"`
	Fail           int    `json:"fail"`
	CurrentAccount string `json:"current_account"`
	StartedAt      string `json:"started_at"`
}

// CronScheduler cron 调度器
type CronScheduler struct {
	db   *DB
	zapi *ZCodeAPI

	stopCh   chan struct{}
	stopOnce sync.Once
	ticker   *time.Ticker
	runMu    sync.Mutex
	running  map[int64]*PlanRunState
	runWG    sync.WaitGroup // 在途 runPlan goroutine（Stop 有界等待）

	tickDone chan struct{} // tick 循环退出标记（先于 runWG 等待）
	stopped  atomic.Bool   // 停机后 goPlan 不再派生

	execMu    sync.Mutex
	execLocks map[int64]*sync.Mutex // per-plan 执行互斥（TryLock，拿不到跳过本 tick）
}

// NewCronScheduler 创建调度器
func NewCronScheduler(db *DB, zapi *ZCodeAPI) *CronScheduler {
	return &CronScheduler{
		db:        db,
		zapi:      zapi,
		stopCh:    make(chan struct{}),
		tickDone:  make(chan struct{}),
		running:   make(map[int64]*PlanRunState),
		execLocks: make(map[int64]*sync.Mutex),
	}
}

// planLock 返回计划级互斥锁
func (s *CronScheduler) planLock(id int64) *sync.Mutex {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	if m, ok := s.execLocks[id]; ok {
		return m
	}
	m := &sync.Mutex{}
	s.execLocks[id] = m
	return m
}

// Start 启动调度器（每分钟检查）
func (s *CronScheduler) Start() {
	s.ticker = time.NewTicker(1 * time.Minute)
	go func() {
		defer close(s.tickDone)
		log.Printf("[scheduler] started, checking every 1 minute")
		for {
			select {
			case <-s.ticker.C:
				s.checkAndRun()
			case <-s.stopCh:
				log.Printf("[scheduler] stopped")
				return
			}
		}
	}()
}

// goPlan 计划执行入口：入 WaitGroup，Stop 时有界等待——停机中断路径的
// 终态写库必须先于 main 的 db.Close，否则计划行永远卡在 running。
// Add 先于 stopped 复核（与 Stop 的 Store→Wait 序配对）：复核失败即退出并
// 返回 false，消灭 check-then-Add 的 Add-after-Wait 竞态窗口
func (s *CronScheduler) goPlan(spawn func()) bool {
	s.runWG.Add(1)
	if s.stopped.Load() {
		s.runWG.Done()
		return false
	}
	go func() {
		defer s.runWG.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[scheduler] plan goroutine panic: %v", r)
			}
		}()
		spawn()
	}()
	return true
}

// Stop 停止调度器（幂等）：关停信号 + 有界等待 tick 与在途 runPlan 收尾。
// 先等 tick 退出再等 runWG：否则 tick 的 goPlan（Add）可能撞上已归零的
// Wait（WaitGroup 契约），且逃逸的计划 goroutine 会无人等待地写已关闭的库
func (s *CronScheduler) Stop() {
	s.stopped.Store(true)
	s.stopOnce.Do(func() {
		if s.ticker != nil {
			s.ticker.Stop()
		}
		close(s.stopCh)
	})
	if s.tickDone != nil {
		select {
		case <-s.tickDone:
		case <-time.After(5 * time.Second):
			log.Printf("[scheduler] stop: tick still running after 5s")
		}
	}
	done := make(chan struct{})
	go func() {
		s.runWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		log.Printf("[scheduler] stop: plan goroutines still running after 15s")
	}
}

// GetRunning 当前运行中的计划状态
func (s *CronScheduler) GetRunning() []PlanRunState {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	out := make([]PlanRunState, 0, len(s.running))
	for _, st := range s.running {
		out = append(out, *st)
	}
	return out
}

func (s *CronScheduler) setRunning(planID int64, st *PlanRunState) {
	s.runMu.Lock()
	if st == nil {
		delete(s.running, planID)
	} else {
		s.running[planID] = st
	}
	s.runMu.Unlock()
}

func (s *CronScheduler) updateRunning(planID int64, fn func(*PlanRunState)) {
	s.runMu.Lock()
	if st, ok := s.running[planID]; ok {
		fn(st)
	}
	s.runMu.Unlock()
}

// checkAndRun 检查活跃计划并执行到期的
func (s *CronScheduler) checkAndRun() {
	plans, err := s.db.ListClaimPlans()
	if err != nil {
		log.Printf("[scheduler] list plans: %v", err)
		return
	}
	now := time.Now()
	for _, plan := range plans {
		if !plan.IsActive {
			continue
		}
		if !shouldRun(plan.CronExpr, now) {
			continue
		}
		// 分钟级去重：本分钟已触发过则跳过（runPlan 以触发时间写 last_run_at）
		if plan.LastRunAt != "" {
			// last_run_at 由 Go 按 '2006-01-02 15:04:05' 本地时间写入，按本地时区解析
			if lastRun, err := time.ParseInLocation("2006-01-02 15:04:05", plan.LastRunAt, time.Local); err == nil {
				if lastRun.Truncate(time.Minute).Equal(now.Truncate(time.Minute)) {
					continue
				}
			}
		}
		s.goPlan(func() { s.executePlan(plan, now) })
	}
}

// RunPlanNow 手动立即执行（UI 触发）；已在执行则拒绝排队。
// 此处一次性获取计划锁并移交所有权给 goroutine，避免先放后抢的空窗被 cron tick 抢走导致静默不执行。
func (s *CronScheduler) RunPlanNow(planID int64) error {
	if s.stopped.Load() {
		return fmt.Errorf("服务停机中，无法执行计划")
	}
	plan, err := s.db.GetClaimPlan(planID)
	if err != nil {
		// 仅"真不存在"报不存在；DB 故障（锁/I/O）如实上抛，避免误导成 404
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("计划不存在: %d", planID)
		}
		return fmt.Errorf("计划查询失败: %w", err)
	}
	lock := s.planLock(planID)
	if !lock.TryLock() {
		return fmt.Errorf("计划正在执行中，请稍后再试")
	}
	if !s.goPlan(func() {
		defer lock.Unlock()
		s.runPlan(plan, time.Now())
	}) {
		// 派生被拒（停机）：锁必须归还，否则该计划此后永久"执行中"
		lock.Unlock()
		return fmt.Errorf("服务停机中，无法执行计划")
	}
	return nil
}

// executePlan 执行计划（cron 路径；已有执行在跑则静默跳过，不排队堆积）。
// triggered 为本次触发的 tick 时间——last_run_at 记它而非执行完成时间，
// 否则跨分钟的执行会让下一分钟的 tick 误判为已跑过而漏跑
func (s *CronScheduler) executePlan(plan *ClaimPlan, triggered time.Time) {
	lock := s.planLock(plan.ID)
	if !lock.TryLock() {
		return
	}
	defer lock.Unlock()
	s.runPlan(plan, triggered)
}

// runPlan 执行计划体（调用方持有计划锁并负责释放；解析目标账号集合）
func (s *CronScheduler) runPlan(plan *ClaimPlan, triggered time.Time) {
	runAt := triggered.Format("2006-01-02 15:04:05")
	// 开始即写触发时间，避免长计划期间被重复触发
	if err := s.db.UpdateClaimPlanRunAt(plan.ID, "running", "执行中", runAt); err != nil {
		log.Printf("[scheduler] plan #%d write last_run_at: %v（写失败可能导致同分钟重复触发）", plan.ID, err)
	}

	targets, err := s.resolveTargets(plan)
	if err != nil {
		if err := s.db.UpdateClaimPlanRunAt(plan.ID, "failed", err.Error(), runAt); err != nil {
			log.Printf("[scheduler] plan #%d write run state: %v", plan.ID, err)
		}
		return
	}
	if len(targets) == 0 {
		if err := s.db.UpdateClaimPlanRunAt(plan.ID, "failed", "没有符合条件的账号", runAt); err != nil {
			log.Printf("[scheduler] plan #%d write run state: %v", plan.ID, err)
		}
		return
	}

	taskType := plan.TaskType
	if taskType == "" {
		taskType = "claim"
	}
	log.Printf("[scheduler] plan #%d %s (%s) start, %d accounts, delay=%ds",
		plan.ID, plan.PlanName, taskType, len(targets), plan.DelaySeconds)

	start := time.Now()
	s.setRunning(plan.ID, &PlanRunState{
		PlanID: plan.ID, PlanName: plan.PlanName, TaskType: taskType,
		Total: len(targets), StartedAt: start.Format("15:04:05"),
	})
	defer s.setRunning(plan.ID, nil)

	var results []string
	successCount, failCount := 0, 0
	aborted := false
	for i, a := range targets {
		if i > 0 && plan.DelaySeconds > 0 {
			// 固定延迟 + 0~50% 随机抖动，模拟人工
			jitter := rand.Intn(plan.DelaySeconds/2 + 1)
			sleep := plan.DelaySeconds + jitter
			log.Printf("[scheduler] plan #%d: sleep %ds before %s", plan.ID, sleep, a.DisplayNameOrEmail())
			// 停机感知：长计划横跨数分钟，Stop() 后不得继续打上游/写库
			select {
			case <-s.stopCh:
				log.Printf("[scheduler] plan #%d: aborted by shutdown before %s", plan.ID, a.DisplayNameOrEmail())
				aborted = true
			case <-time.After(time.Duration(sleep) * time.Second):
			}
			if aborted {
				break
			}
		}
		s.updateRunning(plan.ID, func(st *PlanRunState) { st.CurrentAccount = a.DisplayNameOrEmail() })

		result := s.executeTask(taskType, a)
		if result.OK {
			successCount++
		} else {
			failCount++
		}
		results = append(results, fmt.Sprintf("%s: %s", a.DisplayNameOrEmail(), result.Message))
		done := i + 1
		s.updateRunning(plan.ID, func(st *PlanRunState) {
			st.Done = done
			st.Success = successCount
			st.Fail = failCount
		})
	}

	// 服务停机中断：写终态避免计划卡在 running，且不把半程结果记成 success
	if aborted {
		if err := s.db.UpdateClaimPlanRunAt(plan.ID, "failed", "服务停机中断，本次未完成全部账号", runAt); err != nil {
			log.Printf("[scheduler] plan #%d write run state: %v", plan.ID, err)
		}
		return
	}

	status := "success"
	if successCount == 0 {
		status = "failed"
	}
	summary := fmt.Sprintf("%d/%d 成功: %s", successCount, len(targets), strings.Join(results, "; "))
	// rune 安全截断：中文 3 字节/符，按字节切会切碎 UTF-8 尾巴
	summary = truncate(summary, 900)
	duration := int(time.Since(start).Milliseconds())
	if err := s.db.UpdateClaimPlanRunAt(plan.ID, status, summary, runAt); err != nil {
		log.Printf("[scheduler] plan #%d write run state: %v", plan.ID, err)
	}
	if err := s.db.InsertPlanRunRecord(&PlanRunRecord{
		PlanID: plan.ID, PlanName: plan.PlanName, TaskType: taskType,
		TargetType: plan.TargetType, RunAt: runAt, Status: status, Message: summary,
		Total: len(targets), SuccessCount: successCount, FailCount: failCount,
		DurationMs: duration,
	}); err != nil {
		log.Printf("[scheduler] plan #%d insert run record: %v", plan.ID, err)
	}
	log.Printf("[scheduler] plan #%d done: %s", plan.ID, status)
}

// resolveTargets 按计划目标类型解析账号列表
func (s *CronScheduler) resolveTargets(plan *ClaimPlan) ([]*Account, error) {
	switch plan.TargetType {
	case "single_account":
		a, err := s.db.GetAccount(plan.AccountID)
		if err != nil {
			return nil, err
		}
		// 单账号目标同样受可执行过滤：管理员禁用/无 JWT 的账号不得被
		// cron 自动领活动/消耗重置机会
		runnable := filterRunnableFor(taskTypeOf(plan), []*Account{a})
		if len(runnable) == 0 {
			return nil, fmt.Errorf("目标账号不可执行（已禁用/无效或缺少 JWT）")
		}
		return runnable, nil
	case "group":
		if plan.AccountGroup == "" {
			return nil, fmt.Errorf("分组目标缺少组名")
		}
		all, err := s.db.ListAccounts("")
		if err != nil {
			return nil, err
		}
		// 逗号分隔多组的账号（ListGroups 同约定）按组名逐段匹配
		var inGroup []*Account
		for _, a := range all {
			if matchAccountGroup(a, plan.AccountGroup) {
				inGroup = append(inGroup, a)
			}
		}
		return s.filterAndReport(plan, inGroup), nil
	default: // all_accounts
		all, err := s.db.ListAccounts("")
		if err != nil {
			return nil, err
		}
		return s.filterAndReport(plan, all), nil
	}
}

// filterAndReport 过滤可执行账号并对被剔除的账号留痕——静默丢弃会让
// "5/5 成功"掩盖实际存在但被跳过的账号
func (s *CronScheduler) filterAndReport(plan *ClaimPlan, all []*Account) []*Account {
	runnable := filterRunnableFor(taskTypeOf(plan), all)
	if dropped := len(all) - len(runnable); dropped > 0 {
		log.Printf("[scheduler] plan #%d %s: %d account(s) skipped (disabled/invalid/no JWT)",
			plan.ID, plan.PlanName, dropped)
	}
	return runnable
}

// taskTypeOf 计划任务类型（空缺省 claim）
func taskTypeOf(plan *ClaimPlan) string {
	if plan.TaskType == "" {
		return "claim"
	}
	return plan.TaskType
}

// filterRunnableFor 过滤可执行任务的账号：启用 + 非 invalid/disabled + 有 JWT。
// claim/detect/activate/reset 都以 JWT 通道为主路径，无 JWT 的账号交给它们
// 只会每次运行记一条失败。
func filterRunnableFor(taskType string, in []*Account) []*Account {
	var out []*Account
	for _, a := range in {
		if !a.Enabled || a.Status == StatusDisabled || a.Status == StatusInvalid {
			continue
		}
		if a.ZCodeJWT == "" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// executeTask 按任务类型分发
func (s *CronScheduler) executeTask(taskType string, a *Account) *ClaimResult {
	switch taskType {
	case "detect":
		return s.zapi.DetectForAccount(a)
	case "activate":
		return s.zapi.ActivateForAccount(a)
	case "reset":
		// 重置仅手动触发：自动消耗 5h/周重置窗口可能把宝贵机会浪费在
		// 不需要的时刻。按策略 cron 计划一律拒绝执行 reset，
		// 手动入口（POST /api/accounts/{id}/reset）不受影响。
		log.Printf("[scheduler] reset task refused for %s: resets are manual-only", a.DisplayNameOrEmail())
		return &ClaimResult{Code: -1, Message: "重置已改为仅手动触发：计划不再自动执行 reset"}
	default: // claim
		return s.zapi.ClaimForAccount(a)
	}
}

// ---- Cron 表达式解析（5 段：分 时 日 月 周）----

// shouldRun 判断 cron 表达式在给定时间是否触发
func shouldRun(cronExpr string, now time.Time) bool {
	fields := strings.Fields(strings.TrimSpace(cronExpr))
	if len(fields) != 5 {
		return false
	}
	if !matchField(fields[0], now.Minute(), 0, 59) ||
		!matchField(fields[1], now.Hour(), 0, 23) ||
		!matchField(fields[3], int(now.Month()), 1, 12) {
		return false
	}
	// 标准 cron 语义：日与周均受限（非 *）时任一匹配即触发；否则按 AND
	domOK := matchField(fields[2], now.Day(), 1, 31)
	dowOK := matchField(fields[4], int(now.Weekday()), 0, 6)
	if !isCronStarField(fields[2]) && !isCronStarField(fields[4]) {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// isCronStarField 该字段是否"未受限"（决定 日/周 的 AND/OR 组合）。
// 语义：* 与 */n 前缀算星号位（与 vixie 的首字符判定一致）；含裸 * 的列表
// 也视为未受限（此处比 vixie 的首字符规则更符合直觉：vixie 会把 "9,*"
// 当受限字段）。主流表达式两者一致；分歧仅在混合星号列表这种罕见写法。
func isCronStarField(field string) bool {
	if field == "*" || strings.HasPrefix(field, "*/") {
		return true
	}
	if strings.Contains(field, ",") {
		for _, p := range strings.Split(field, ",") {
			if strings.TrimSpace(p) == "*" {
				return true
			}
		}
	}
	return false
}

func matchField(field string, value, min, max int) bool {
	if field == "*" {
		return true
	}
	// 逗号分支必须在 */ 前缀分支之前：否则 "*/3,15" 被当成步进值 "3,15"
	// 解析失败，表达式通过校验却永远不触发（与 validateCronField 同序）
	if strings.Contains(field, ",") {
		// 列表内每部分递归回 matchField：支持 "1-5,20"、"*/3,15" 等混合写法
		for _, p := range strings.Split(field, ",") {
			if matchField(strings.TrimSpace(p), value, min, max) {
				return true
			}
		}
		return false
	}
	if strings.HasPrefix(field, "*/") {
		// 与 matchStep 同一锚点语义：*/n 从字段下限起算（日域 1,1+n,1+2n…），
		// 而非 value%n==0（会得到 5,10,15… 且月末不齐）
		step, err := strconv.Atoi(field[2:])
		if err != nil || step <= 0 {
			return false
		}
		return (value-min)%step == 0
	}
	if strings.Contains(field, "/") {
		return matchStep(field, value, min, max)
	}
	if strings.Contains(field, "-") {
		return matchRange(field, value)
	}
	return matchSinglePart(field, value)
}

func matchSinglePart(part string, value int) bool {
	part = strings.TrimSpace(part)
	if part == "*" {
		return true
	}
	n, err := strconv.Atoi(part)
	if err != nil {
		return false
	}
	return n == value
}

func matchRange(field string, value int) bool {
	parts := strings.SplitN(field, "-", 2)
	if len(parts) != 2 {
		return false
	}
	start, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	end, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return false
	}
	return value >= start && value <= end
}

func matchStep(field string, value, min, max int) bool {
	parts := strings.SplitN(field, "/", 2)
	if len(parts) != 2 {
		return false
	}
	rangePart := strings.TrimSpace(parts[0])
	step, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || step <= 0 {
		return false
	}
	var start, end int
	if rangePart == "*" {
		start, end = min, max
	} else if strings.Contains(rangePart, "-") {
		rp := strings.SplitN(rangePart, "-", 2)
		start, err = strconv.Atoi(strings.TrimSpace(rp[0]))
		if err != nil {
			return false
		}
		end, err = strconv.Atoi(strings.TrimSpace(rp[1]))
		if err != nil {
			return false
		}
	} else {
		start, err = strconv.Atoi(rangePart)
		if err != nil {
			return false
		}
		end = max
	}
	if value < start || value > end {
		return false
	}
	return (value-start)%step == 0
}

// ValidateCronExpr 校验 cron 表达式
func ValidateCronExpr(expr string) error {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return fmt.Errorf("cron 表达式必须为 5 段（分 时 日 月 周），当前 %d 段", len(fields))
	}
	ranges := []struct{ min, max int }{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	labels := []string{"分钟", "小时", "日", "月", "周"}
	for i, f := range fields {
		if err := validateCronField(f, ranges[i].min, ranges[i].max); err != nil {
			return fmt.Errorf("%s字段: %w", labels[i], err)
		}
	}
	return nil
}

func validateCronField(field string, min, max int) error {
	field = strings.TrimSpace(field)
	if field == "" {
		return fmt.Errorf("空字段")
	}
	if field == "*" {
		return nil
	}
	// 先拆列表再验每部分（与 matchField 的 ,优先 顺序一致）：
	// 否则 "*/3,15" 会被当成步进值 "3,15" 而误拒
	if strings.Contains(field, ",") {
		for _, p := range strings.Split(field, ",") {
			if err := validateCronField(p, min, max); err != nil {
				return err
			}
		}
		return nil
	}
	if strings.Contains(field, "/") {
		parts := strings.SplitN(field, "/", 2)
		if len(parts) != 2 {
			return fmt.Errorf("步进格式错误")
		}
		step, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || step <= 0 {
			return fmt.Errorf("步进值无效")
		}
		rangePart := strings.TrimSpace(parts[0])
		if rangePart == "*" {
			return nil
		}
		return validateCronField(rangePart, min, max)
	}
	if strings.Contains(field, "-") {
		parts := strings.SplitN(field, "-", 2)
		if len(parts) != 2 {
			return fmt.Errorf("范围格式错误")
		}
		start, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		end, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil {
			return fmt.Errorf("范围无效: %s", field)
		}
		if start < min || end > max || start > end {
			return fmt.Errorf("范围 %d-%d 超出 [%d-%d]", start, end, min, max)
		}
		return nil
	}
	n, err := strconv.Atoi(field)
	if err != nil {
		return fmt.Errorf("无效数字: %s", field)
	}
	if n < min || n > max {
		return fmt.Errorf("值 %d 超出 [%d-%d]", n, min, max)
	}
	return nil
}

// NextRunTime 计算下次触发时间（前端展示）
func NextRunTime(cronExpr string, from time.Time) time.Time {
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := t.Add(366 * 24 * time.Hour)
	for t.Before(limit) {
		if shouldRun(cronExpr, t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}
