// 周期归并、推断周期与额度估算。
package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const cycWeek = 7 * 86400

// cycAddEvent 按默认值写入一条请求，挂在来源的默认限额上。
func cycAddEvent(t *testing.T, ctx *Context, source string, ts int64) {
	t.Helper()
	mustAddEvent(t, ctx, accountSource(source), testEvent(fmt.Sprint(ts), float64(ts), "f", "claude-opus-5", weekLimitID, 1_000_000))
}

// cycKnownCycle 返回来源第一个有读数（非推断）的周期。
func cycKnownCycle(t *testing.T, ctx *Context, src Source) Cycle {
	t.Helper()
	cycles, err := listCycles(ctx.DB, src)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cycles {
		if !c.Inferred {
			return c
		}
	}
	t.Fatal("没有带读数的周期")
	return Cycle{}
}

func cycReport(t *testing.T, ctx *Context, src Source, c Cycle, byAgent, bySession bool) *CycleReport {
	t.Helper()
	q, err := loadCycleQuota(ctx.DB, src, c)
	if err != nil {
		t.Fatal(err)
	}
	r, err := cycleReport(ctx.DB, ctx, src, c, q, byAgent, bySession)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEstimateQuota(t *testing.T) {
	ratio := func(v float64) *float64 { return &v }
	cases := []struct {
		name      string
		points    []readingPoint
		wantNil   bool
		method    string
		estimate  float64
		estAbs    float64 // 0 表示按 approx 的默认容差
		ratio     *float64
		wantRange []float64 // nil 表示期望没有取整区间
	}{
		{
			// 周期开头已有 10% 不在本机日志里：比值口径会被带偏，增量口径不受影响
			name:      "读数跨度足够时取增量口径",
			points:    []readingPoint{{used: 10, cost: 0}, {used: 11, cost: 10}, {used: 12, cost: 20}, {used: 15, cost: 50}},
			method:    "delta",
			estimate:  1000.0,
			ratio:     ratio(333.33),
			wantRange: []float64{312.5, 333.33},
		},
		{
			name:      "上跳跨度不足 3 点时只用比值口径",
			points:    []readingPoint{{used: 10, cost: 0}, {used: 11, cost: 10}, {used: 12, cost: 20}},
			method:    "ratio",
			estimate:  166.67,
			wantRange: []float64{153.85, 166.67},
		},
		{
			// 两段的上跳跨度各是 1 和 2 点，加起来满 3 点
			name: "各段上跳跨度合计满 3 点才给增量口径",
			points: []readingPoint{
				{used: 10, cost: 0}, {used: 11, cost: 10}, {used: 12, cost: 20},
				{used: 2, cost: 100, drop: true}, {used: 3, cost: 110}, {used: 4, cost: 120}, {used: 5, cost: 130},
			},
			method:   "delta",
			estimate: 1000.0,
			ratio:    ratio(333.33),
		},
		{
			// 小数读数没有取整区间
			name:     "退回比值口径",
			points:   []readingPoint{{used: 30.6432, cost: 187.94}},
			method:   "ratio",
			estimate: 613.32,
			estAbs:   0.01,
		},
		{name: "用量为零", points: []readingPoint{{used: 0, cost: 5}}, wantNil: true},
		{
			// 34 → 2 之间花的 700 横跨清零，两段都不计；不分段时 ratio 会是 1300/22、delta 只有回落前的一段
			name: "回落把周期切成两段",
			points: []readingPoint{
				{used: 10, cost: 0}, {used: 20, cost: 100}, {used: 30, cost: 200},
				{used: 2, cost: 900, drop: true}, {used: 12, cost: 1100}, {used: 22, cost: 1300},
			},
			method:   "delta",
			estimate: 1500.0,
			ratio:    ratio(1200.0),
		},
		{
			// 回落后还没上涨：只有回落前一段可用（不分段时最新读数为 0，给不出估算）
			name:     "回落后尚无进展",
			points:   []readingPoint{{used: 10, cost: 0}, {used: 30, cost: 200}, {used: 0, cost: 500, drop: true}},
			method:   "ratio",
			estimate: 666.67,
		},
		{
			// 首条就是回落：起点取这条读数，之前的金额不计
			name:     "首条读数即回落",
			points:   []readingPoint{{used: 2, cost: 400, drop: true}, {used: 12, cost: 600}},
			method:   "ratio",
			estimate: 2000.0,
		},
		{name: "只有回落读数", points: []readingPoint{{used: 30, cost: 200}, {used: 2, cost: 300, drop: true}, {used: 2, cost: 320}}, method: "ratio", estimate: 666.67},
		{name: "没有读数", points: []readingPoint{}, wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			est := estimateQuota(tc.points)
			if tc.wantNil {
				if est != nil {
					t.Fatalf("期望 nil，实际 %+v", est)
				}
				return
			}
			if est == nil {
				t.Fatal("期望有估算，实际 nil")
			}
			if est.Method != tc.method {
				t.Errorf("method = %q，期望 %q", est.Method, tc.method)
			}
			okEst := approx(est.Estimate, tc.estimate)
			if tc.estAbs > 0 {
				okEst = approxAbs(est.Estimate, tc.estimate, tc.estAbs)
			}
			if !okEst {
				t.Errorf("estimate = %v，期望 %v", est.Estimate, tc.estimate)
			}
			if tc.ratio != nil && !approx(est.Ratio, *tc.ratio) {
				t.Errorf("ratio = %v，期望 %v", est.Ratio, *tc.ratio)
			}
			checkRatioRange(t, est.RatioRange, tc.wantRange)
		})
	}
}

// checkRatioRange 比对取整区间；want 为 nil 表示期望没有区间。
func checkRatioRange(t *testing.T, got, want []float64) {
	if want == nil {
		if got != nil {
			t.Errorf("ratio_range = %v，期望 nil", got)
		}
		return
	}
	if len(got) != len(want) {
		t.Fatalf("ratio_range = %v，期望 %v", got, want)
	}
	for i := range want {
		if !approx(got[i], want[i]) {
			t.Errorf("ratio_range = %v，期望 %v", got, want)
		}
	}
}

func TestUnanchoredWindowRollsIntoOneCycle(t *testing.T) {
	ctx := newTestContext(t)
	index := ctx.Cycles
	canon := func(resets int64, used float64) int64 {
		t.Helper()
		v, err := index.Canonical("claude", "seven_day", 10080, resets, used)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	key := [2]string{"claude", "seven_day"}
	first := canon(1790000000, 0)
	if got := canon(1790003600, 0); got != 1790003600 { // 未锚定时随请求滚动
		t.Fatalf("未锚定周期没有随请求滚动: %d", got)
	}
	anchored := canon(1790007200, 1.0)
	if got := canon(1790007201, 2.0); got != anchored {
		t.Fatalf("抖动 1 秒的读数归到了 %d，期望 %d", got, anchored)
	}
	if n := len(index.known[key]); n != 1 || first == anchored {
		t.Fatalf("known=%d first=%d anchored=%d，期望 1 个周期且锚定后键改变", n, first, anchored)
	}
	// 官方提前重置：上一周期已有用量，新的重置时间开新周期
	if got := canon(anchored+3*86400, 0); got == anchored {
		t.Fatalf("提前重置后仍归到旧周期 %d", got)
	}
	if n := len(index.known[key]); n != 2 {
		t.Fatalf("known=%d，期望 2", n)
	}
}

func TestRequestsFallIntoKnownAndInferredCycles(t *testing.T) {
	ctx := newTestContext(t)
	src := sources["claude"]
	var resets int64 = 1_790_000_000
	begin := resets - cycWeek
	for _, ts := range []int64{begin - 10*86400, begin - 86400, begin + 3600, begin + 7200, resets + 3600} {
		cycAddEvent(t, ctx, "claude", ts)
	}
	mustAddReading(t, ctx, "claude", "seven_day", 10080, float64(begin+5000), 10.0, resets)
	mustAddReading(t, ctx, "claude", "seven_day", 10080, float64(begin+8000), 20.0, resets)
	cycles, err := listCycles(ctx.DB, src)
	if err != nil {
		t.Fatal(err)
	}
	var reports []*CycleReport
	type cycShape struct {
		inferred bool
		requests int
	}
	var shape []cycShape
	for _, c := range cycles {
		r := cycReport(t, ctx, src, c, false, false)
		reports = append(reports, r)
		shape = append(shape, cycShape{r.Inferred, r.Requests})
	}
	// 新到旧：周期之后 1 条（推断）、有读数的周期 2 条、往前推两个 7 天各 1 条（推断）
	want := []cycShape{{true, 1}, {false, 2}, {true, 1}, {true, 1}}
	if !reflect.DeepEqual(shape, want) {
		t.Fatalf("周期结构 %+v，期望 %+v", shape, want)
	}
	known := reports[1]
	if known.UsedPercent == nil || *known.UsedPercent != 20.0 || !approx(known.Cost, 10.0) {
		t.Fatalf("有读数周期 used=%v cost=%v，期望 20 与 10", known.UsedPercent, known.Cost)
	}
	// 第一个读数时只发生了 1 条请求（$5），第二个读数时 2 条
	if known.QuotaEstimate == nil || !approx(known.QuotaEstimate.Ratio, 50.0) {
		t.Fatalf("quota_estimate = %+v，期望 ratio 50", known.QuotaEstimate)
	}
	for _, r := range reports {
		if r.Inferred && r.QuotaEstimate != nil {
			t.Errorf("推断周期不应有额度估算: %+v", r.QuotaEstimate)
		}
	}
}

// 只有读数、还没有请求的账号（刚加的号）也列出周期；只有请求、没有读数覆盖的落进推断周期。
func TestListCyclesFromReadingsOrEventsAlone(t *testing.T) {
	ctx := newTestContext(t)
	if _, err := ctx.AddReading("work", weekLimitID, weekMinutes, now()-60, 5, int64(now())+86400, nil); err != nil {
		t.Fatal(err)
	}
	syncAddEvent(t, ctx, "claude", "m", 60)
	for account, inferred := range map[string]bool{"work": false, "claude": true} {
		cycles, err := listCycles(ctx.DB, accountSource(account))
		if err != nil || len(cycles) != 1 || cycles[0].Inferred != inferred || cycles[0].LimitID != weekLimitID {
			t.Errorf("%s 的周期 = %+v %v", account, cycles, err)
		}
	}
}

func TestQuotaReferenceUsesWellUsedCycles(t *testing.T) {
	quota := func(used, estimate *float64) *cycleQuota {
		q := &cycleQuota{used: used}
		if estimate != nil {
			q.estimate = &QuotaEstimate{Estimate: *estimate}
		}
		return q
	}
	f := floatp
	var picked []float64
	// 用量不足 30% 的、没有读数的（推断周期）不进参考
	for _, q := range []*cycleQuota{quota(f(16), f(1386)), quota(f(91), f(1169)), quota(f(91), f(1317)),
		quota(f(41), f(1307)), quota(nil, nil), quota(f(5), f(100))} {
		if est, ok := q.reference(); ok {
			picked = append(picked, est)
		}
	}
	got := quotaReference(picked)
	want := &QuotaReference{Median: 1307, Cycles: 3, MinUsedPercent: 30}
	if got == nil || *got != *want {
		t.Fatalf("quotaReference = %+v，期望 %+v", got, want)
	}
	if got := quotaReference(nil); got != nil {
		t.Fatalf("没有够格的周期时应为 nil: %+v", got)
	}
}

func TestByAgentBreakdownSumsToCycle(t *testing.T) {
	ctx := newTestContext(t)
	src := sources["claude"]
	var resets int64 = 1_790_000_000
	for i, ae := range [][2]string{{"main", "high"}, {"main", "high"}, {"reviewer", "medium"}} {
		ev := testEvent(fmt.Sprintf("m%d", i), float64(resets-3600+int64(i)), "f"+ae[0], "claude-opus-5", "seven_day", 1_000_000)
		ev.Agent, ev.Effort = strp(ae[0]), strp(ae[1])
		mustAddEvent(t, ctx, src, ev)
	}
	mustAddReading(t, ctx, "claude", "seven_day", 10080, float64(resets-60), 10.0, resets)
	cycle := cycKnownCycle(t, ctx, src)
	report := cycReport(t, ctx, src, cycle, true, false)
	if report.Agents == nil {
		t.Fatal("开了 by_agent 却没有 agents")
	}
	agents := map[[2]string]AgentRow{}
	sum := 0.0
	for _, a := range *report.Agents {
		agents[[2]string{cycStr(a.Agent), cycStr(a.Effort)}] = a
		sum += a.Cost
	}
	mainHigh, ok := agents[[2]string{"main", "high"}]
	if !ok || mainHigh.Requests != 2 || mainHigh.Sessions != 1 {
		t.Fatalf("(main, high) = %+v，期望 2 条请求、1 个会话", mainHigh)
	}
	if mainHigh.Share == nil || !approxAbs(*mainHigh.Share, 2.0/3, 1e-4) {
		t.Fatalf("(main, high) share = %v，期望 2/3", mainHigh.Share)
	}
	if !approx(sum, report.Cost) {
		t.Fatalf("角色金额合计 %v ≠ 周期金额 %v", sum, report.Cost)
	}
	if r := cycReport(t, ctx, src, cycle, false, false); r.Agents != nil {
		t.Fatal("没开 by_agent 不应有 agents")
	}
}

func cycStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestCycleReportBySession(t *testing.T) {
	type cycEv struct {
		file   string
		agent  string
		tokens int64
	}
	type cycSessionWant struct {
		session   string
		agent     string
		subagents int
		requests  int64
	}
	cases := []struct {
		name      string
		source    string
		limitID   string
		model     string
		ukey      string
		events    []cycEv
		want      []cycSessionWant // 金额降序
		wantShare map[string]float64
		wantQuota map[string]float64
		textHas   []string
	}{
		{
			name:    "Claude 子代理归入所在主会话",
			source:  "claude",
			limitID: "seven_day",
			model:   "claude-opus-5",
			ukey:    "m",
			events: []cycEv{
				{"proj/s1.jsonl", "main", 1_000_000},
				{"proj/s1/subagents/agent-a.jsonl", "worker", 1_000_000},
				{"proj/s1/subagents/agent-b.jsonl", "reviewer", 1_000_000},
				{"proj/s2.jsonl", "main", 1_000_000},
			},
			want:      []cycSessionWant{{"proj/s1", "main", 2, 3}, {"proj/s2", "main", 0, 1}},
			wantShare: map[string]float64{"proj/s1": 3.0 / 4},
			// ratio 口径：周期金额 / 10% 为总额度，s1 占其 7.5%
			wantQuota: map[string]float64{"proj/s1": 7.5},
			textHas:   []string{"按会话", "2 子代理", "≈7.5% 额度"},
		},
		{
			// 不在 <会话>/subagents/ 下的子代理日志自成一个会话，名字后标角色
			name:    "子代理日志不在 subagents 目录下时单列并标角色",
			source:  "claude-b",
			limitID: "seven_day",
			model:   "claude-opus-5",
			ukey:    "b",
			events: []cycEv{
				{"proj/s1.jsonl", "main", 2_000_000},
				{"proj/agent-x.jsonl", "worker", 1_000_000},
			},
			want:    []cycSessionWant{{"proj/s1", "main", 0, 1}, {"proj/agent-x", "worker", 0, 1}},
			textHas: []string{"agent-x" + strings.Repeat(" ", 17) + " worker"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newTestContext(t)
			src := accountSource(tc.source)
			var resets int64 = 1_790_000_000
			for i, e := range tc.events {
				ev := testEvent(fmt.Sprintf("%s%d", tc.ukey, i), float64(resets-3600+int64(i)), e.file, tc.model, tc.limitID, e.tokens)
				ev.Agent = strp(e.agent)
				mustAddEvent(t, ctx, src, ev)
			}
			mustAddReading(t, ctx, tc.source, tc.limitID, 10080, float64(resets-60), 10.0, resets)
			cycle := cycKnownCycle(t, ctx, src)
			report := cycReport(t, ctx, src, cycle, false, true)
			if report.Sessions == nil {
				t.Fatal("开了 by_session 却没有 sessions")
			}
			sessions := *report.Sessions
			var got []cycSessionWant
			byName := map[string]SessionRow{}
			sum := 0.0
			for _, s := range sessions {
				got = append(got, cycSessionWant{s.Session, cycStr(s.Agent), s.Subagents, s.Requests})
				byName[s.Session] = s
				sum += s.Cost
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sessions = %+v，期望 %+v", got, tc.want)
			}
			checkSessionPercents(t, byName, tc.wantShare, tc.wantQuota)
			if !approx(sum, report.Cost) {
				t.Errorf("会话金额合计 %v ≠ 周期金额 %v", sum, report.Cost)
			}
			if r := cycReport(t, ctx, src, cycle, false, false); r.Sessions != nil {
				t.Error("没开 by_session 不应有 sessions")
			}
			result := &Result{Sources: map[string]*SourceBlock{
				tc.source: {Currency: "USD", Symbol: "$", Cycles: []*CycleReport{report}},
			}, Warnings: []string{}}
			text := renderText(result)
			for _, s := range tc.textHas {
				if !strings.Contains(text, s) {
					t.Errorf("文本缺少 %q:\n%s", s, text)
				}
			}
		})
	}
}

// checkSessionPercents 比对各会话的金额占比与额度百分比；byName 按会话名索引报告里的会话行。
func checkSessionPercents(t *testing.T, byName map[string]SessionRow, wantShare, wantQuota map[string]float64) {
	for name, w := range wantShare {
		if s := byName[name].Share; s == nil || !approxAbs(*s, w, 1e-4) {
			t.Errorf("%s share = %v，期望 %v", name, s, w)
		}
	}
	for name, w := range wantQuota {
		if q := byName[name].QuotaPercent; q == nil || !approxAbs(*q, w, 0.01) {
			t.Errorf("%s quota_percent = %v，期望 %v", name, q, w)
		}
	}
}

// status 只给进行中的周期出完整报告，别的周期只算额度估算、凑够参考周期数就停：参考额度与列出的进行中周期
// 都须与 history（每个周期都出完整报告）算出来的相同。造 11 个带读数的周期（各周期估算不同，其中一个用得不够），
// 加一个更早的推断周期。
func TestStatusMatchesHistory(t *testing.T) {
	ctx := newTestContext(t)
	current := int64(now()) + 3*86400 // 进行中的周期 3 天后重置
	for k := range int64(11) {
		resets := current - k*cycWeek
		begin := resets - cycWeek
		cycAddEvent(t, ctx, "claude", begin+3600)
		cycAddEvent(t, ctx, "claude", begin+7200)
		used := 30 + float64(k)*5
		if k == 3 {
			used = 25
		}
		mustAddReading(t, ctx, "claude", "seven_day", 10080, float64(begin+5000), 10, resets)
		mustAddReading(t, ctx, "claude", "seven_day", 10080, float64(begin+8000), used, resets)
	}
	cycAddEvent(t, ctx, "claude", current-12*cycWeek-86400)
	block := func(cmd string) map[string]any {
		return cliSources(t, cliRun(t, ctx, cmd, "--offline", "--source", "claude"))["claude"].(map[string]any)
	}
	status, history := block("status"), block("history")
	ref, ok := history["quota_reference"].(map[string]any)
	if !ok || ref["cycles"] != float64(referenceCycles) {
		t.Fatalf("history 的参考额度 = %v，want 取满 %d 个周期", history["quota_reference"], referenceCycles)
	}
	if !reflect.DeepEqual(status["quota_reference"], ref) {
		t.Errorf("status 的参考额度 %v，history 的 %v", status["quota_reference"], ref)
	}
	sc, hc := status["cycles"].([]any), history["cycles"].([]any)
	if len(hc) != 12 || len(sc) != 1 || !reflect.DeepEqual(sc[0], hc[0]) {
		t.Errorf("status 列出 %d 个周期，history %d 个；status 的应只有 history 的第一个（进行中）", len(sc), len(hc))
	}
}
