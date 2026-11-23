package main

import (
	"strings"
	"testing"
)

// record 在同一周期内读数回落（官方中途清零或重算档位）时给那一行带上 dropped_from：入库保留这个标记，
// 周期报告据此分段估算额度并列出回落。

func TestReadingDropIsStoredFromReadingLog(t *testing.T) {
	ctx := newTestContext(t)
	line := func(ts int64, used float64) obj {
		return obj{"ts": ts, "window": "seven_day", "used_percentage": used, "resets_at": 1790442000}
	}
	writeJSONL(t, ctx.Args.rateLimitLog("claude"),
		line(1790000100, 34.0),
		with(line(1790000200, 2.0), obj{"dropped_from": 34.0}),
		with(line(1790000300, 3.0), obj{"dropped_from": "34"}), // 不是数字：整行跳过
	)
	if err := sources["claude"].CollectReadings(ctx); err != nil {
		t.Fatal(err)
	}
	rows := mustQuery(t, ctx, "SELECT used_percent, dropped_from FROM readings ORDER BY ts")
	if len(rows) != 2 || !rows[0].IsNull("dropped_from") || rows[1].Float("dropped_from") != 34.0 {
		t.Fatalf("readings = %v，期望 34 无标记、2 带 dropped_from=34", rows)
	}
	if len(ctx.Warnings) != 1 || !strings.Contains(ctx.Warnings[0], ctx.Args.rateLimitLog("claude")+" 第 3 行") {
		t.Errorf("warnings = %q，期望第 3 行被跳过", ctx.Warnings)
	}
}

func TestCycleReportSplitsAtDrop(t *testing.T) {
	ctx := newTestContext(t)
	src := sources["claude"]
	var resets int64 = 1_790_000_000
	begin := resets - cycWeek
	for i := int64(1); i <= 6; i++ { // 每条请求 $5
		cycAddEvent(t, ctx, "claude", begin+i*1000)
	}
	mustAddReading(t, ctx, "claude", "seven_day", 10080, float64(begin+1500), 10.0, resets)
	mustAddReading(t, ctx, "claude", "seven_day", 10080, float64(begin+2500), 20.0, resets)
	if _, err := ctx.AddReading("claude", "seven_day", 10080, float64(begin+4500), 2.0, resets, floatp(20.0)); err != nil {
		t.Fatal(err)
	}
	mustAddReading(t, ctx, "claude", "seven_day", 10080, float64(begin+6500), 12.0, resets)
	cycles, err := listCycles(ctx.DB, src)
	if err != nil {
		t.Fatal(err)
	}
	var r *CycleReport
	for _, c := range cycles {
		if !c.Inferred {
			r = cycReport(t, ctx, src, c, false, false)
		}
	}
	if r == nil {
		t.Fatal("没有有读数的周期")
	}
	if r.UsedPercent == nil || *r.UsedPercent != 12.0 {
		t.Errorf("used_percent = %v，期望最新读数 12", r.UsedPercent)
	}
	// 回落前一段 $10 / 20%，回落后一段 $10 / 10%；20% → 2% 之间的 $10 跨过清零，不计。
	// 不分段时会是 $30 / 12% = 250
	if r.QuotaEstimate == nil || r.QuotaEstimate.Method != "ratio" || !approx(r.QuotaEstimate.Estimate, 66.67) {
		t.Fatalf("quota_estimate = %+v，期望 ratio 66.67", r.QuotaEstimate)
	}
	at := isoOf(float64(begin + 4500))
	if len(r.Drops) != 1 || r.Drops[0] != (ReadingDrop{At: at, From: 20, To: 2}) {
		t.Fatalf("drops = %+v，期望一次 20 → 2", r.Drops)
	}
	text := strings.Join(renderCycle(r, "$"), "\n")
	if want := "  周期内回落  " + at[:16] + "  20% → 2%"; !strings.Contains(text, want) {
		t.Errorf("文本里没有 %q：\n%s", want, text)
	}
}
