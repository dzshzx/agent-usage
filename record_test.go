package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"testing"
	"time"

	"github.com/dzshzx/claude-switch/account"

	"github.com/dzshzx/agent-usage/reading"
)

// 额度写法的覆盖面在 reading 包的测试里；这里只要两种写法各一份最小的。
const (
	weekInfo  = `{"unifiedWindows":{"seven_day":{"utilization":0.9,"resetsAt":1795313110}}}`
	weekUsage = `{"rate_limits_available":true,"rate_limits":{"seven_day":{"utilization":40,"resets_at":"2026-11-22T02:05:10Z"}}}`
)

// recordHome 把 HOME 指到临时目录（claude-switch 的切换记录与非账号目标都按 HOME 找），并清掉 CLAUDE_CONFIG_DIR。
func recordHome(t *testing.T, current string, targets ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if current != "" {
		writeFile(t, filepath.Join(home, account.StateDir, "current"), current+"\n")
	}
	for _, name := range targets {
		writeFile(t, filepath.Join(home, account.TargetsDir, name+".json"), "{}")
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
	for _, in := range []string{weekInfo, weekInfo, strings.Replace(weekInfo, `"utilization":0.9`, `"utilization":0.91`, 1)} {
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
	if err := runRecord(ctx.Args, strings.NewReader(weekUsage), time.Unix(1795300000, 0)); err != nil {
		t.Fatal(err)
	}
	if n := len(readLines(t, ctx.Args.rateLimitLog("claude"))); n != 1 {
		t.Errorf("主号记了 %d 行，期望 1", n)
	}
}

func TestRecordSkipsSettingsTargets(t *testing.T) {
	ctx := newTestContext(t)
	recordHome(t, "gateway", "gateway")
	if err := runRecord(ctx.Args, strings.NewReader(weekInfo), time.Unix(1795300000, 0)); err != nil {
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
	if err := runRecord(ctx.Args, strings.NewReader(`{"type":"assistant"}`), time.Now()); !errors.Is(err, reading.ErrNotReading) {
		t.Errorf("err = %v，期望 reading.ErrNotReading", err)
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
