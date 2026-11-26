// --text 输出：用覆盖各分支的结果对照保存的样本文本，改写渲染代码时逐字不变。
package main

import (
	"fmt"
	"strings"
	"testing"
)

func rendModelRow(model *string, tier string, cost float64, pricedAs *string, unpriced int64) ModelRow {
	return ModelRow{
		Model:             model,
		Tier:              tier,
		Requests:          12,
		InputTokens:       1_234_567,
		CachedInputTokens: 987_654,
		OutputTokens:      4_321,
		Cost:              cost,
		PricedAs:          pricedAs,
		UnpricedRequests:  unpriced,
	}
}

func rendAgentRow(agent, model, effort *string, cost float64) AgentRow {
	return AgentRow{
		Agent:             agent,
		Model:             model,
		Effort:            effort,
		Tier:              "standard",
		Sessions:          3,
		Requests:          7,
		InputTokens:       2_500_000_000,
		CachedInputTokens: 1_500,
		OutputTokens:      999,
		Cost:              cost,
	}
}

// rendCycle 是样本用的默认周期；调用方再覆盖字段。
func rendCycle() *CycleReport {
	return &CycleReport{
		LimitID:        "seven_day",
		Inferred:       false,
		Active:         true,
		WindowStartsAt: "2026-10-17T08:00:00+08:00",
		ResetsAt:       "2026-10-24T08:00:00+08:00",
		UsedPercent:    floatp(42.5),
		UsedPercentAt:  strp("2026-10-20T14:35:07+08:00"),
		Cost:           123.456,
		QuotaEstimate:  &QuotaEstimate{Estimate: 290.5, Method: "delta", Readings: 9},
		Models: []ModelRow{
			rendModelRow(strp("claude-opus-5-very-long-model-name"), "fast", 100.0, strp("Claude Opus 5"), 2),
			rendModelRow(nil, "standard", 23.456, nil, 0),
		},
	}
}

func rendSampleResult() *Result {
	agents := []AgentRow{
		rendAgentRow(strp("main"), strp("claude-opus-5"), strp("high"), 80.0),
		rendAgentRow(nil, nil, nil, 43.456),
	}
	c1 := rendCycle()
	c1.Agents = &agents
	c1.Sessions = &[]SessionRow{
		{Session: "/home/u/.claude/projects/中文会话目录/会话甲乙丙丁戊己庚辛壬癸-0123456789abcdef", Agent: strp("reviewer"),
			FirstRequestAt: "2026-10-18T09:05:00+08:00", LastRequestAt: "2026-10-18T10:45:30+08:00", Subagents: 2, Requests: 15,
			InputTokens: 12_345, CachedInputTokens: 0, OutputTokens: 678, Cost: 1234.567, Share: floatp(0.255), QuotaPercent: floatp(7.5)},
		{Session: "short", Agent: strp("main"), FirstRequestAt: "2026-10", LastRequestAt: "", Requests: 1, Cost: 0.004},
	}

	c2 := rendCycle()
	c2.Inferred = true
	c2.Active = false
	c2.UsedPercent = nil
	c2.UsedPercentAt = nil
	c2.QuotaEstimate = nil
	c2.Cost = 0
	agents2 := []AgentRow{rendAgentRow(strp("reviewer"), strp("claude-sonnet-5"), strp("low"), 0)}
	c2.Agents = &agents2

	c3 := rendCycle()
	c3.Active = false

	day1 := &DayReport{Date: "2026-10-22", Cost: 12.5, Requests: 30, InputTokens: 999, OutputTokens: 1_000}
	day2 := &DayReport{Date: "2026-10-23", Cost: 50.0, Requests: 70, InputTokens: 5_000_000, OutputTokens: 2}
	claude := &SourceBlock{
		Currency:       "USD",
		Symbol:         "$",
		Cycles:         []*CycleReport{c1, c2, c3},
		QuotaReference: &QuotaReference{Median: 1301.25, Cycles: 4, MinUsedPercent: 30},
		Daily:          []*DayReport{day1, day2},
		PricesTable: []PriceEntry{
			{Model: "claude-opus-4-1", Tier: "standard", Input: 15.0, CachedInput: floatp(1.5), Output: 75.0},
			{Model: "claude-haiku", Tier: "fast", Input: 0.8, CachedInput: nil, Output: 4.0},
			{Model: "claude-sonnet-4-5", Tier: "standard", Input: 3.0, CachedInput: floatp(3.0), Output: 15.0},
			{
				Model: "claude-haiku-5-5", Tier: "standard", Input: 0.1, CachedInput: floatp(0.01), Output: 0.5,
				LongInput: floatp(0.5), LongCachedInput: floatp(0.05), LongOutput: floatp(2.5), LongThreshold: intp(100_000),
			},
		},
	}
	zeroDay := *day1
	zeroDay.Cost = 0.0
	zeroDays := &SourceBlock{Currency: "USD", Symbol: "$", Daily: []*DayReport{&zeroDay}}

	srcs := map[string]*SourceBlock{"claude": claude, "claude-b": zeroDays}
	return &Result{Sources: srcs, Warnings: []string{"claude: 示例警告", "claude-b: 第二条"}}
}

// rendLineDiff 逐行列出两段文本不同的行，便于定位。
func rendLineDiff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	var b strings.Builder
	for i := 0; i < max(len(g), len(w)); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			fmt.Fprintf(&b, "第 %d 行\n  got:  %q\n  want: %q\n", i+1, gl, wl)
		}
	}
	if len(g) != len(w) {
		fmt.Fprintf(&b, "行数 got=%d want=%d\n", len(g), len(w))
	}
	return b.String()
}

func TestRenderTextMatchesSavedSample(t *testing.T) {
	got := renderText(rendSampleResult())
	if updateGolden(t, "render-text.txt", got+"\n") {
		return
	}
	want := strings.TrimRight(fixtureText(t, "render-text.txt"), "\n")
	if got != want {
		t.Errorf("渲染结果与样本不一致:\n%s", rendLineDiff(got, want))
	}
}

func TestRenderTextWithoutSourcesListsOnlyWarnings(t *testing.T) {
	got := renderText(&Result{Sources: map[string]*SourceBlock{}, Warnings: []string{"x"}})
	if got != "warning: x" {
		t.Errorf("renderText = %q，期望 %q", got, "warning: x")
	}
}
