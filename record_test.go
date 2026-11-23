package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// rate_limit_info 与 get_usage 应答的写法取自 Claude Code 2.1.328 的 stream-json 输出（Zeta 的 helper/accounts 实测）。
const (
	sampleInfo  = `{"status":"allowed","resetsAt":1795057510,"rateLimitType":"five_hour","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.25,"resetsAt":1795057510,"status":"allowed"},"seven_day":{"utilization":0.9,"resetsAt":1795313110,"status":"allowed"}}}`
	sampleUsage = `{"rate_limits_available":true,"subscription_type":"max","rate_limits":{"five_hour":{"utilization":25,"resets_at":"2026-11-19T03:05:10.000Z"},"seven_day":{"utilization":40,"resets_at":"2026-11-22T02:05:10.000Z"}}}`
	// sampleLive 是 Claude Code 2.1.330 的 stream-json 实录（uuid 与 session_id 换成了假值）。
	sampleLive = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1795885200,"rateLimitType":"seven_day","utilization":0.9,"overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"surpassedThreshold":0.75,"unifiedWindows":{"seven_day":{"utilization":0.9,"resetsAt":1795885200,"status":"allowed_warning","surpassedThreshold":0.75}},"overageEnabled":false},"uuid":"00000000-0000-4000-8000-000000000001","session_id":"00000000-0000-4000-8000-000000000002"}`
)

func TestParseReadingAcceptsTheThreeShapes(t *testing.T) {
	usageReset, _ := time.Parse(time.RFC3339, "2026-11-22T02:05:10Z")
	for _, c := range []struct {
		name, in string
		want     weekReading
	}{
		{"stream-json 整行", `{"type":"rate_limit_event","rate_limit_info":` + sampleInfo + `,"session_id":"s"}`, weekReading{90, 1795313110}},
		{"只有 rate_limit_info", sampleInfo, weekReading{90, 1795313110}},
		{"get_usage 应答", sampleUsage, weekReading{40, usageReset.Unix()}},
		{"2.1.330 实录：只带 7 天窗口、额度告警", sampleLive, weekReading{90, 1795885200}},
	} {
		got, ok, err := parseReading([]byte(c.in))
		if err != nil || !ok || got != c.want {
			t.Errorf("%s: = %+v, %v, %v，期望 %+v", c.name, got, ok, err, c.want)
		}
	}
}

func TestParseReadingWithoutWeekWindowRecordsNothing(t *testing.T) {
	for _, in := range []string{
		`{"unifiedWindows":{"five_hour":{"utilization":0.25,"resetsAt":1795057510}}}`,
		`{"rate_limits_available":false}`,
		`{"rate_limits_available":true,"rate_limits":{"seven_day":null}}`,
	} {
		if _, ok, err := parseReading([]byte(in)); ok || err != nil {
			t.Errorf("%s: ok=%v err=%v，期望不记也不报错", in, ok, err)
		}
	}
}

func TestParseReadingRejectsOtherJSON(t *testing.T) {
	for _, in := range []string{`{"type":"assistant"}`, `[1]`, `not json`, `null`, `{"rate_limit_info":3}`} {
		if _, _, err := parseReading([]byte(in)); !errors.Is(err, errNotReading) {
			t.Errorf("%s: err = %v，期望 errNotReading", in, err)
		}
	}
}

func TestNextReadingKeepsOnlyChanges(t *testing.T) {
	const resets = 1_790_442_000
	last := readingLine{TS: 1, Window: "seven_day", Used: 50, ResetsAt: resets}
	drop := 50.0
	for _, c := range []struct {
		name    string
		r       weekReading
		last    readingLine
		ok      bool
		used    float64
		resets  int64
		dropped *float64
	}{
		{"头一份", weekReading{10, resets}, readingLine{}, true, 10, resets, nil},
		{"同一窗口升高", weekReading{51, resets + 1}, last, true, 51, resets, nil},
		{"同一窗口没变", weekReading{50, resets}, last, false, 0, 0, nil},
		{"乱序到达小幅回落", weekReading{48, resets}, last, false, 0, 0, nil},
		{"中途清零", weekReading{2, resets}, last, true, 2, resets, &drop},
		{"新窗口", weekReading{1, resets + 7*86400}, last, true, 1, resets + 7*86400, nil},
		{"旧窗口迟到", weekReading{99, resets - 7*86400}, last, false, 0, 0, nil},
	} {
		got, ok := nextReading(c.r, 100, c.last)
		if ok != c.ok {
			t.Errorf("%s: ok = %v，期望 %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Used != c.used || got.ResetsAt != c.resets || got.TS != 100 || got.Window != "seven_day" ||
			(got.DroppedFrom == nil) != (c.dropped == nil) || (c.dropped != nil && *got.DroppedFrom != *c.dropped) {
			t.Errorf("%s: = %+v", c.name, got)
		}
	}
}

// recordHome 把 HOME 指到临时目录（claude-switch 的切换记录与非账号目标都按 HOME 找），并清掉 CLAUDE_CONFIG_DIR。
func recordHome(t *testing.T, current string, targets ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if current != "" {
		writeFile(t, filepath.Join(home, ".local/state/claude-switch/current"), current+"\n")
	}
	for _, name := range targets {
		writeFile(t, filepath.Join(home, switchTargetsDir, name+".json"), "{}")
	}
	return home
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestRecordWritesToTheCurrentAccount(t *testing.T) {
	ctx := newTestContext(t)
	recordHome(t, "claude-b")
	now := time.Unix(1795300000, 0)
	for _, in := range []string{sampleInfo, sampleInfo, strings.Replace(sampleInfo, `"utilization":0.9`, `"utilization":0.91`, 1)} {
		if err := runRecord(ctx.Args, strings.NewReader(in), now); err != nil {
			t.Fatal(err)
		}
	}
	got := readLines(t, ctx.Args.rateLimitLog("claude-b"))
	want := []string{
		`{"ts":1795300000,"window":"seven_day","used_percentage":90,"resets_at":1795313110}`,
		`{"ts":1795300000,"window":"seven_day","used_percentage":91,"resets_at":1795313110}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("claude-b 的读数 = %q，期望 %q（第二份没变，不记）", got, want)
	}
	if lines := readLines(t, ctx.Args.rateLimitLog("claude")); lines != nil {
		t.Errorf("主号不该有读数：%q", lines)
	}
}

func TestRecordAccountFlagOverridesTheSwitchRecord(t *testing.T) {
	ctx := newTestContext(t)
	recordHome(t, "claude-b")
	ctx.Args.Account = "claude"
	if err := runRecord(ctx.Args, strings.NewReader(sampleUsage), time.Unix(1795300000, 0)); err != nil {
		t.Fatal(err)
	}
	if n := len(readLines(t, ctx.Args.rateLimitLog("claude"))); n != 1 {
		t.Errorf("主号记了 %d 行，期望 1", n)
	}
}

func TestRecordSkipsSettingsTargets(t *testing.T) {
	ctx := newTestContext(t)
	recordHome(t, "gateway", "gateway")
	if err := runRecord(ctx.Args, strings.NewReader(sampleInfo), time.Unix(1795300000, 0)); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(ctx.Args.DataDir, "rate-limits*.jsonl"))
	if len(matches) != 0 {
		t.Errorf("非账号目标不该有读数：%v", matches)
	}
}

func TestRecordRejectsUnknownInput(t *testing.T) {
	ctx := newTestContext(t)
	recordHome(t, "")
	if err := runRecord(ctx.Args, strings.NewReader(`{"type":"assistant"}`), time.Now()); !errors.Is(err, errNotReading) {
		t.Errorf("err = %v，期望 errNotReading", err)
	}
}

// 记下的读数经 status 读出来：已用百分比与额度估算都用上它。
func TestRecordedReadingFeedsTheCycleReport(t *testing.T) {
	ctx := newTestContext(t)
	recordHome(t, "")
	var resets int64 = 1_790_000_000
	for i := int64(1); i <= 6; i++ { // 每条请求 $5，共 $30
		cycAddEvent(t, ctx, "claude", resets-cycWeek+i*1000)
	}
	info := `{"unifiedWindows":{"seven_day":{"utilization":0.3,"resetsAt":1790000000}}}`
	if err := runRecord(ctx.Args, strings.NewReader(info), time.Unix(resets-cycWeek+7000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := sources["claude"].CollectReadings(ctx); err != nil {
		t.Fatal(err)
	}
	rows := mustQuery(t, ctx, "SELECT used_percent, resets_at FROM readings WHERE source='claude'")
	if len(rows) != 1 || rows[0].Float("used_percent") != 30 || rows[0].Int("resets_at") != resets {
		t.Fatalf("readings = %v，期望一行 30%%、重置于 %d", rows, resets)
	}
}

func TestHookSessionStartRegistersTheSession(t *testing.T) {
	ctx := newTestContext(t)
	now := time.Unix(1795300000, 500_000_000)
	recordHome(t, "claude-b")
	runHook(ctx.Args, strings.NewReader(`{"session_id":"s1","hook_event_name":"SessionStart","source":"startup"}`), &bytes.Buffer{}, now)
	recordHome(t, "gateway", "gateway")
	runHook(ctx.Args, strings.NewReader(`{"session_id":"s1","source":"resume"}`), &bytes.Buffer{}, now.Add(time.Second))
	var stderr bytes.Buffer
	runHook(ctx.Args, strings.NewReader(`{"source":"startup"}`), &stderr, now)
	if !strings.Contains(stderr.String(), "没有 session_id") {
		t.Errorf("stderr = %q，期望说明没有 session_id", stderr.String())
	}
	got := readLines(t, ctx.Args.sessionAccountLog())
	want := []string{
		`{"ts":1795300000.5,"session_id":"s1","account":"claude-b"}`,
		`{"ts":1795300001.5,"session_id":"s1","account":"gateway","target":true}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("登记 = %q，期望 %q", got, want)
	}
	// 写出的登记由入库那边按同一格式读回
	rows := map[string][]accountRow{}
	for _, line := range got {
		if err := primary.loadAccountLine([]byte(line), rows); err != nil {
			t.Fatal(err)
		}
	}
	if r := rows[jsonText("s1")]; len(r) != 2 || r[0].account != "claude-b" || r[1].account != targetSource {
		t.Errorf("读回 = %+v，期望 claude-b 再 %s", r, targetSource)
	}
}

func TestHookOutputsNothing(t *testing.T) {
	ctx := newTestContext(t)
	recordHome(t, "")
	var stdout, stderr bytes.Buffer
	in, _ := json.Marshal(map[string]string{"session_id": "s2"})
	os.Stdin = mustPipe(t, in)
	argv := append([]string{"hook", "session-start"}, cliPathArgs(ctx)...)
	if code := run(argv, &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("退出码 %d、stdout %q、stderr %q，期望 0 且都为空", code, stdout.String(), stderr.String())
	}
	if n := len(readLines(t, ctx.Args.sessionAccountLog())); n != 1 {
		t.Errorf("登记了 %d 行，期望 1", n)
	}
}

// mustPipe 返回一个读出 data 后结束的文件，测试里替换 os.Stdin 用；用完由 t.Cleanup 换回。
func mustPipe(t *testing.T, data []byte) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	t.Cleanup(func() { os.Stdin = old; r.Close() })
	return r
}

// 几个程序同时 record：读 .state、追加、写 .state 由锁串成一整步，所以每一行都按它的上一行判断：要么比上一行高、
// 不带 dropped_from，要么 dropped_from 正是上一行的百分比；.state 是最后一行。没有锁时会有两次 record 读到同一份
// .state，记下的行就对不上它的上一行。
func TestConcurrentRecordsStayOrdered(t *testing.T) {
	ctx := newTestContext(t)
	const n = 20
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := weekReading{used: float64(i), resetsAt: 1_790_442_000}
			if _, err := recordReading(ctx.Args, "claude", r, time.Unix(1790000000, 0)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	lines := readLines(t, ctx.Args.rateLimitLog("claude"))
	prev := 0.0
	for i, l := range lines {
		var r readingLine
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("行 %q 读不出：%v", l, err)
		}
		switch {
		case r.DroppedFrom == nil && r.Used > prev:
		case r.DroppedFrom != nil && *r.DroppedFrom == prev && r.Used <= prev-readingDropMin:
		default:
			t.Fatalf("第 %d 行对不上上一行 %v：%q", i+1, prev, lines)
		}
		prev = r.Used
	}
	state, err := os.ReadFile(strings.TrimSuffix(ctx.Args.rateLimitLog("claude"), ".jsonl") + ".state")
	if err != nil || string(state) != lines[len(lines)-1] {
		t.Errorf(".state = %q，期望最后一行 %q", state, lines[len(lines)-1])
	}
}
