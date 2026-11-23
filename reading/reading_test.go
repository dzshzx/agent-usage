package reading

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dzshzx/claude-switch/account"
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
		want     Reading
	}{
		{"stream-json 整行", `{"type":"rate_limit_event","rate_limit_info":` + sampleInfo + `,"session_id":"s"}`, Reading{Used: 90, ResetsAt: 1795313110}},
		{"只有 rate_limit_info", sampleInfo, Reading{Used: 90, ResetsAt: 1795313110}},
		{"get_usage 应答", sampleUsage, Reading{Used: 40, ResetsAt: usageReset.Unix()}},
		{"2.1.330 实录：只带 7 天窗口、额度告警", sampleLive, Reading{Used: 90, ResetsAt: 1795885200}},
	} {
		got, ok, err := Parse([]byte(c.in))
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
		if _, ok, err := Parse([]byte(in)); ok || err != nil {
			t.Errorf("%s: ok=%v err=%v，期望不记也不报错", in, ok, err)
		}
	}
}

func TestParseReadingRejectsOtherJSON(t *testing.T) {
	for _, in := range []string{`{"type":"assistant"}`, `[1]`, `not json`, `null`, `{"rate_limit_info":3}`, `{"unifiedWindows":null}`} {
		if _, _, err := Parse([]byte(in)); !errors.Is(err, ErrNotReading) {
			t.Errorf("%s: err = %v，期望 ErrNotReading", in, err)
		}
	}
}

func TestNextReadingKeepsOnlyChanges(t *testing.T) {
	const resets = 1_790_442_000
	last := Line{TS: 1, Window: "seven_day", Used: 50, ResetsAt: resets}
	drop := 50.0
	for _, c := range []struct {
		name    string
		r       Reading
		last    Line
		ok      bool
		used    float64
		resets  int64
		dropped *float64
	}{
		{"头一份", Reading{Used: 10, ResetsAt: resets}, Line{}, true, 10, resets, nil},
		{"同一窗口升高", Reading{Used: 51, ResetsAt: resets + 1}, last, true, 51, resets, nil},
		{"同一窗口没变", Reading{Used: 50, ResetsAt: resets}, last, false, 0, 0, nil},
		{"乱序到达小幅回落", Reading{Used: 48, ResetsAt: resets}, last, false, 0, 0, nil},
		{"中途清零", Reading{Used: 2, ResetsAt: resets}, last, true, 2, resets, &drop},
		{"新窗口", Reading{Used: 1, ResetsAt: resets + 7*86400}, last, true, 1, resets + 7*86400, nil},
		{"旧窗口迟到", Reading{Used: 99, ResetsAt: resets - 7*86400}, last, false, 0, 0, nil},
	} {
		got, ok := Next(c.r, 100, c.last)
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

// 几个程序同时 record：读 .state、追加、写 .state 由锁串成一整步，所以每一行都按它的上一行判断：要么比上一行高、
// 不带 dropped_from，要么 dropped_from 正是上一行的百分比；.state 是最后一行。没有锁时会有两次 record 读到同一份
// .state，记下的行就对不上它的上一行。
func TestConcurrentRecordsStayOrdered(t *testing.T) {
	dir := t.TempDir()
	const n = 20
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := Reading{Used: float64(i), ResetsAt: 1_790_442_000}
			if _, err := Append(dir, "claude", r, time.Unix(1790000000, 0)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	lines := readLines(t, LogPath(dir, "claude"))
	prev := 0.0
	for i, l := range lines {
		var r Line
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("行 %q 读不出：%v", l, err)
		}
		switch {
		case r.DroppedFrom == nil && r.Used > prev:
		case r.DroppedFrom != nil && *r.DroppedFrom == prev && r.Used <= prev-DropMin:
		default:
			t.Fatalf("第 %d 行对不上上一行 %v：%q", i+1, prev, lines)
		}
		prev = r.Used
	}
	state, err := os.ReadFile(strings.TrimSuffix(LogPath(dir, "claude"), ".jsonl") + ".state")
	if err != nil || string(state) != lines[len(lines)-1] {
		t.Errorf(".state = %q，期望最后一行 %q", state, lines[len(lines)-1])
	}
}

// Record 按 home 下 claude-switch 的切换记录选账号，记进 home 下的数据目录；非账号目标不记。
func TestRecordFollowsTheSwitchRecordUnderHome(t *testing.T) {
	t.Setenv("AGENT_USAGE_DATA_DIR", "")
	home := t.TempDir()
	now := time.Unix(1795300000, 0)
	if ok, err := Record(home, []byte(sampleInfo), now); !ok || err != nil {
		t.Fatalf("没有切换记录：Record = %v, %v，期望记到主号", ok, err)
	}
	writeFile(t, filepath.Join(home, account.StateDir, "current"), "claude-b\n")
	if ok, err := Record(home, []byte(sampleUsage), now); !ok || err != nil {
		t.Fatalf("claude-b：Record = %v, %v", ok, err)
	}
	writeFile(t, filepath.Join(home, account.StateDir, "current"), "gateway\n")
	writeFile(t, filepath.Join(home, account.TargetsDir, "gateway.json"), "{}")
	if ok, err := Record(home, []byte(sampleInfo), now); ok || err != nil {
		t.Fatalf("非账号目标：Record = %v, %v，期望不记", ok, err)
	}
	if _, err := Record(home, []byte(`{"type":"assistant"}`), now); !errors.Is(err, ErrNotReading) {
		t.Errorf("不认得的 JSON：err = %v，期望 ErrNotReading", err)
	}
	dir := filepath.Join(home, ".local/share/agent-usage")
	if n, m := len(readLines(t, LogPath(dir, "claude"))), len(readLines(t, LogPath(dir, "claude-b"))); n != 1 || m != 1 {
		t.Errorf("主号 %d 行、claude-b %d 行，期望各 1 行", n, m)
	}
	if _, err := os.Stat(LogPath(dir, "gateway")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("非账号目标不该有读数文件：%v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
