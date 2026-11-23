// 各测试文件共用的夹具：空库上的入库上下文（价格已从样本页载入）、合成日志的写入与计数。
package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain：测试不读本机真实的 hook 目录、claude-switch 状态与同步配置，也不经 ssh 连真实主机。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-usage-test-")
	if err != nil {
		panic(err)
	}
	for k, v := range map[string]string{
		"CLAUDE_SWITCH_STATE_DIR": filepath.Join(dir, "claude-switch"),
		"AGENT_USAGE_CONFIG":      filepath.Join(dir, "config.json"),
		"AGENT_USAGE_DATA_DIR":    filepath.Join(dir, "data"),
		"CLAUDE_CONFIG_DIR":       filepath.Join(dir, "claude"),
	} {
		os.Setenv(k, v)
	}
	remoteRun = func(*Args, string, []string, func(io.WriteCloser, io.Reader) error) error {
		panic("测试里不能经 ssh 连真实主机：先替换 remoteRun")
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fixturePath 返回 testdata/ 下样本文件的绝对路径。
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func fixtureText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(fixturePath(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fileURL(path string) string { return "file://" + path }

// updateGolden 在 UPDATE_GOLDEN=1 时把 testdata/<name> 改写成 content 并返回 true，调用方据此跳过比对。
// 重录：UPDATE_GOLDEN=1 go test -run <测试名> .，提交前看 git diff。
func updateGolden(t *testing.T, name, content string) bool {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		return false
	}
	if err := os.WriteFile(fixturePath(t, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return true
}

// newTestContext 建一个空库上的入库上下文，Claude 价格已从样本页载入（各账号共用）；Args 指向临时目录下的各路径。
// 账号表清回只有主号，别的账号在用到时登记。
func newTestContext(t *testing.T) *Context {
	t.Helper()
	resetSources()
	dir := t.TempDir()
	db, err := openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var warnings []string
	ok, err := refreshPrices(db, sources["claude"], fileURL(fixturePath(t, "anthropic-pricing.md")), &warnings)
	if err != nil || !ok {
		t.Fatalf("载入 claude 价格失败: ok=%v err=%v %v", ok, err, warnings)
	}
	args := &Args{
		Command:        "status",
		Days:           14,
		MaxPriceAge:    defaultMaxPriceAgeDays,
		Offline:        true,
		ClaudeHome:     filepath.Join(dir, "claude"),
		SwitchStateDir: filepath.Join(dir, "claude-switch"),
		ConfigPath:     filepath.Join(dir, "config.json"),
		DataDir:        dir,
	}
	ctx, err := newContext(db, args)
	if err != nil {
		t.Fatal(err)
	}
	ctx.localSynced = true // 本机的用量由各测试自己写入或显式入库，同步时不另外扫一遍
	return ctx
}

// writeJSONL 把若干记录按 JSON Lines 写到 path（父目录自动建立），返回 path。
func writeJSONL(t *testing.T, path string, records ...any) string {
	t.Helper()
	var b strings.Builder
	for _, r := range records {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	writeFile(t, path, b.String())
	return path
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

// obj 是合成日志记录的简写。
type obj = map[string]any

// count 返回表里满足 where 的行数。
func count(t *testing.T, ctx *Context, table, where string) int64 {
	t.Helper()
	if where == "" {
		where = "1"
	}
	v, err := ctx.DB.Scalar("SELECT COUNT(*) FROM " + table + " WHERE " + where)
	if err != nil {
		t.Fatal(err)
	}
	return toInt(v)
}

// claudeMessage 是 Claude Code 日志里一条 assistant 消息。
func claudeMessage(msgID string, out int64, ts, model string) obj {
	return obj{
		"type":      "assistant",
		"timestamp": ts,
		"message": obj{
			"id":    msgID,
			"model": model,
			"usage": obj{
				"input_tokens":                2,
				"cache_read_input_tokens":     1000,
				"cache_creation_input_tokens": 300,
				"cache_creation":              obj{"ephemeral_5m_input_tokens": 100, "ephemeral_1h_input_tokens": 200},
				"output_tokens":               out,
				"speed":                       "standard",
			},
		},
	}
}

// with 返回在 base 之上覆盖若干字段的新记录（不改 base）。
func with(base obj, extra obj) obj {
	out := make(obj, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// without 返回去掉某个字段的新记录。
func without(base obj, key string) obj {
	out := make(obj, len(base))
	for k, v := range base {
		if k != key {
			out[k] = v
		}
	}
	return out
}

func mustParseTs(t *testing.T, s string) float64 {
	t.Helper()
	ts, err := parseTs(s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// approx 判断两个浮点数在容差内相等：相对 1e-6 加绝对 1e-12。
func approx(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	tol := 1e-12
	if r := 1e-6 * absFloat(b); r > tol {
		tol = r
	}
	return diff <= tol
}

func approxAbs(a, b, abs float64) bool {
	return absFloat(a-b) <= abs
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// testEvent 构造一条测试用请求，未给的字段取默认值。
func testEvent(ukey string, ts float64, file, model, limitID string, inputTokens int64) *Event {
	ev := &Event{Ukey: ukey, Ts: ts, File: file, Model: strp(model), Tier: "standard", InputTokens: inputTokens}
	if limitID != "" {
		ev.LimitID = strp(limitID)
	}
	return ev
}

func mustAddEvent(t *testing.T, ctx *Context, src Source, ev *Event) bool {
	t.Helper()
	added, err := ctx.AddEvent(src, ev)
	if err != nil {
		t.Fatal(err)
	}
	return added
}

func mustAddReading(t *testing.T, ctx *Context, source, limitID string, window int64, ts, used float64, resets int64) {
	t.Helper()
	if _, err := ctx.AddReading(source, limitID, window, ts, used, resets, nil); err != nil {
		t.Fatal(err)
	}
}

func mustSync(t *testing.T, ctx *Context, name string) SyncStats {
	t.Helper()
	stats, err := accountSource(name).Sync(ctx)
	if err != nil {
		t.Fatalf("%s sync: %v", name, err)
	}
	return stats
}

func mustQuery(t *testing.T, ctx *Context, query string, args ...any) []Row {
	t.Helper()
	rows, err := ctx.DB.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
