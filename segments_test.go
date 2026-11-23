// segments 子命令：切段边界、逐段的金额拆分、上下文分档、子代理与分组、活跃小时，以及命令行的参数校验。
package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestSegmentBounds(t *testing.T) {
	now := time.Date(2026, 11, 7, 18, 0, 0, 0, time.Local)
	unix := func(s string) float64 {
		tm, err := parseLocalTime(s)
		if err != nil {
			t.Fatal(err)
		}
		return float64(tm.Unix())
	}
	got, err := segmentBounds("", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1]-got[0] != 7*86400 || got[1] != float64(now.Unix()) {
		t.Fatalf("缺省边界 = %v，期望 until 前 7 天到现在", got)
	}
	got, err = segmentBounds("2026-11-04", "2026-11-07 12:00", []string{"2026-11-06T19:55", "2026-11-05 13:32"}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{unix("2026-11-04"), unix("2026-11-05 13:32"), unix("2026-11-06 19:55"), unix("2026-11-07 12:00")}
	if len(got) != len(want) {
		t.Fatalf("边界 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("边界 = %v，期望 %v（切点按时间排序）", got, want)
		}
	}
	for _, tc := range []struct {
		since, until string
		splits       []string
		msg          string
	}{
		{"2026-11-05", "2026-11-04", nil, "早于"},
		{"2026-11-04", "2026-11-06", []string{"2026-11-04"}, "不在"},
		{"2026-11-04", "2026-11-06", []string{"2026-11-07"}, "不在"},
		{"2026-11-04", "2026-11-06", []string{"2026-11-05 10:00", "2026-11-05T10:00"}, "重复"},
		{"昨天", "", nil, "无法解析"},
	} {
		if _, err := segmentBounds(tc.since, tc.until, tc.splits, now); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("segmentBounds(%q, %q, %v) 错误 = %v，期望含「%s」", tc.since, tc.until, tc.splits, err, tc.msg)
		}
	}
}

// 样本价（Claude Opus 5，每 1M）：输入 5、读缓存 0.5、5 分钟写缓存 6.25、输出 25。
func TestSegmentReportSplitsCostContextAndSubagents(t *testing.T) {
	ctx := newTestContext(t)
	src := sources["claude"]
	base := mustParseTs(t, "2026-09-02T00:00:00Z")
	main := testEvent("a", base, "p/s.jsonl", "claude-opus-5", "", 150_000)
	main.CachedInputTokens, main.CacheWrite5mTokens, main.OutputTokens = 100_000, 40_000, 1_000
	main.Agent, main.Effort = strp("main"), strp("high")
	sub := testEvent("b", base+60, "p/s/subagents/agent-x.jsonl", "claude-opus-5", "", 600_000)
	sub.CachedInputTokens, sub.OutputTokens = 600_000, 2_000
	sub.Agent = strp("general-purpose")
	unpriced := testEvent("c", base+7200, "p/t.jsonl", "mystery-model", "", 50_000)
	for _, ev := range []*Event{main, sub, unpriced} {
		mustAddEvent(t, ctx, src, ev)
	}
	book, err := ctx.Book(src, false)
	if err != nil {
		t.Fatal(err)
	}
	segs, err := segmentsReport(ctx.DB, book, src, []float64{base, base + 3600, base + 10800})
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("段数 = %d，期望 2", len(segs))
	}
	s := segs[0]
	// main: 10K 新输入 0.05 + 100K 读缓存 0.05 + 40K 写缓存 0.25 + 1K 输出 0.025 = 0.375
	// sub: 600K 读缓存 0.30 + 2K 输出 0.05 = 0.35
	if s.Requests != 2 || !approx(s.Cost, 0.725) || s.UnpricedRequests != 0 {
		t.Fatalf("段 1 请求 %d 金额 %v 未定价 %d，期望 2、0.725、0", s.Requests, s.Cost, s.UnpricedRequests)
	}
	p := s.CostParts
	if !approx(p.FreshInput, 0.05) || !approx(p.CacheRead, 0.35) || !approx(p.CacheWrite, 0.25) || !approx(p.Output, 0.075) {
		t.Fatalf("金额拆分 = %+v", p)
	}
	if !approx(p.FreshInput+p.CacheRead+p.CacheWrite+p.Output, s.Cost) {
		t.Fatalf("拆分之和 %v 不等于总金额 %v", p.FreshInput+p.CacheRead+p.CacheWrite+p.Output, s.Cost)
	}
	if *s.AvgContextTokens != 375_000 || !approxAbs(*s.CacheHitRatio, 700_000.0/750_000, 1e-4) {
		t.Fatalf("平均上下文 %v 缓存命中 %v", *s.AvgContextTokens, *s.CacheHitRatio)
	}
	b := s.ContextBuckets
	if len(b) != 6 || b[1].Requests != 1 || b[5].Requests != 1 || b[5].MaxTokens != nil || b[5].MinTokens != 500_000 {
		t.Fatalf("上下文分档 = %+v，期望 150K 落 100–200K、600K 落 500K+", b)
	}
	if !approxAbs(*b[5].CostShare, 0.35/0.725, 1e-4) {
		t.Fatalf("500K+ 金额占比 = %v", *b[5].CostShare)
	}
	if s.Sessions != 1 || s.Subagents != 1 || !approx(s.SubagentCost, 0.35) || !approx(*s.CostPerSubagent, 0.35) {
		t.Fatalf("会话 %d 子代理 %d 金额 %v", s.Sessions, s.Subagents, s.SubagentCost)
	}
	if len(s.Agents) != 2 || *s.Agents[0].Key != "main" || *s.Agents[1].Key != "general-purpose" {
		t.Fatalf("按角色 = %+v，期望 main 在前（金额降序）", s.Agents)
	}
	if len(s.Efforts) != 2 || *s.Efforts[0].Key != "high" || s.Efforts[1].Key != nil || *s.Efforts[1].OutputPerRequest != 2000 {
		t.Fatalf("按 effort = %+v，期望 high 与取不到 effort 的一组", s.Efforts)
	}
	if s.ActiveHours != 0 || s.CostPerActiveHour != nil {
		t.Fatalf("2 次请求不应算活跃小时：%d", s.ActiveHours)
	}
	u := segs[1]
	if u.Requests != 1 || u.UnpricedRequests != 1 || u.Cost != 0 || u.SubagentShare != nil {
		t.Fatalf("段 2 = %+v，期望只有一条未定价请求", u)
	}
}

func TestSegmentActiveHours(t *testing.T) {
	ctx := newTestContext(t)
	src := sources["claude"]
	base := mustParseTs(t, "2026-09-02T00:00:00Z")
	for i := range activeHourMinRequests {
		ev := testEvent("busy"+string(rune('a'+i)), base+float64(i*60), "p/s.jsonl", "claude-opus-5", "", 0)
		ev.OutputTokens = 1_000 // 每次 0.025
		mustAddEvent(t, ctx, src, ev)
	}
	lone := testEvent("lone", base+3*3600, "p/s.jsonl", "claude-opus-5", "", 0)
	lone.OutputTokens = 100_000
	mustAddEvent(t, ctx, src, lone)
	book, _ := ctx.Book(src, false)
	segs, err := segmentsReport(ctx.DB, book, src, []float64{base, base + 86400})
	if err != nil {
		t.Fatal(err)
	}
	s := segs[0]
	if s.ActiveHours != 1 || !approx(*s.CostPerActiveHour, 0.5) || *s.RequestsPerActiveHour != float64(activeHourMinRequests) {
		t.Fatalf("活跃小时 %d 每小时 %v %v，期望 1、0.5（零星那次不计入）", s.ActiveHours, *s.CostPerActiveHour, *s.RequestsPerActiveHour)
	}
}

func TestSegmentsCommandLine(t *testing.T) {
	ctx := newTestContext(t)
	out := cliRun(t, ctx, "segments", "--offline", "--no-sync", "--since", "2026-09-01", "--until", "2026-09-03",
		"--split", "2026-09-02")
	for name, raw := range cliSources(t, out) {
		segs, ok := raw.(map[string]any)["segments"].([]any)
		if !ok || len(segs) != 2 {
			t.Errorf("%s 的 segments = %#v，期望两段", name, raw.(map[string]any)["segments"])
		}
	}
	for _, argv := range [][]string{
		{"status", "--split", "2026-09-02"},
		{"segments", "--since", "2026-09-03", "--until", "2026-09-01"},
		{"segments", "--since", "2026-09-01", "--until", "2026-09-03", "--split", "2026-09-04"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append(argv, "--offline", "--no-sync", "--data-dir", t.TempDir()), &stdout, &stderr); code != 2 {
			t.Errorf("%v 退出码 = %d，期望 2；stderr: %s", argv, code, stderr.String())
		}
	}
}
