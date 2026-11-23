// 日志行的取数规则：哪些行入库、入库的数是多少、哪些行按坏行跳过。这里固定的是入库结果，不是报错原文。
package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type raw = json.RawMessage

// claudeLine 是 claudeMessage 的一行日志，edit 可就地改记录、message 与 usage（值可用 raw 写原文）。
func claudeLine(t *testing.T, edit func(rec, msg, usage obj)) string {
	t.Helper()
	rec := claudeMessage("m1", 10, "2026-09-02T00:00:00Z", "claude-opus-5")
	msg := rec["message"].(obj)
	edit(rec, msg, msg["usage"].(obj))
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// eventTokens 是一条请求入库后的 input、cached、5m、1h、output、reasoning。
type eventTokens [6]int64

func TestClaudeLineAcceptanceRules(t *testing.T) {
	base := eventTokens{1302, 1000, 100, 200, 10, 0}
	usage := func(key, value string) func(rec, msg, usage obj) {
		return func(_, _, u obj) { u[key] = raw(value) }
	}
	message := func(key, value string) func(rec, msg, usage obj) {
		return func(_, m, _ obj) { m[key] = raw(value) }
	}
	record := func(key, value string) func(rec, msg, usage obj) {
		return func(r, _, _ obj) { r[key] = raw(value) }
	}
	cases := []struct {
		name string
		edit func(rec, msg, usage obj)
		line string       // 不为空时直接用这一行
		want *eventTokens // 入库时的计数；nil 表示不入库
		bad  bool         // 不入库时是否按坏行跳过
	}{
		{name: "原样", edit: func(_, _, _ obj) {}, want: &base},
		{name: "input 空串当 0", edit: usage("input_tokens", `""`), want: &eventTokens{1300, 1000, 100, 200, 10, 0}},
		{name: "input false 当 0", edit: usage("input_tokens", `false`), want: &eventTokens{1300, 1000, 100, 200, 10, 0}},
		{name: "input true 当 1", edit: usage("input_tokens", `true`), want: &eventTokens{1301, 1000, 100, 200, 10, 0}},
		{name: "input null 当 0", edit: usage("input_tokens", `null`), want: &eventTokens{1300, 1000, 100, 200, 10, 0}},
		{name: "input 空数组当 0", edit: usage("input_tokens", `[]`), want: &eventTokens{1300, 1000, 100, 200, 10, 0}},
		{name: "input 空对象当 0", edit: usage("input_tokens", `{}`), want: &eventTokens{1300, 1000, 100, 200, 10, 0}},
		{name: "input 小数向零截断", edit: usage("input_tokens", `2.9`), want: &base},
		{name: "input 指数写法", edit: usage("input_tokens", `2e0`), want: &base},
		{name: "output 带 .0", edit: usage("output_tokens", `10.0`), want: &base},
		{name: "input 数字字符串", edit: usage("input_tokens", `"5"`), bad: true},
		{name: "input 非空数组", edit: usage("input_tokens", `[1]`), bad: true},
		{name: "5m 为 null 时取总写入减 1h", edit: func(_, _, u obj) {
			u["cache_creation_input_tokens"] = 500
			u["cache_creation"] = raw(`{"ephemeral_5m_input_tokens": null, "ephemeral_1h_input_tokens": 200}`)
		}, want: &eventTokens{1502, 1000, 300, 200, 10, 0}},
		{name: "5m 为数字字符串", edit: usage("cache_creation", `{"ephemeral_5m_input_tokens": "7"}`), bad: true},
		{name: "cache_creation 空数组当空对象", edit: usage("cache_creation", `[]`), want: &eventTokens{1302, 1000, 300, 0, 10, 0}},
		{name: "cache_creation 是字符串", edit: usage("cache_creation", `"x"`), bad: true},
		{name: "thinking 计入 reasoning", edit: usage("output_tokens_details", `{"thinking_tokens": 7}`), want: &eventTokens{1302, 1000, 100, 200, 10, 7}},
		{name: "output_tokens_details 是数字", edit: usage("output_tokens_details", `5`), bad: true},
		{name: "usage 空数组", edit: message("usage", `[]`)},
		{name: "usage 非空数组", edit: message("usage", `[1]`), bad: true},
		{name: "message 为 0", edit: record("message", `0`)},
		{name: "message 是字符串", edit: record("message", `"usage"`), bad: true},
		{name: "id 是数字", edit: message("id", `123`), bad: true},
		{name: "id 空串", edit: message("id", `""`)},
		{name: "model 是数字", edit: message("model", `123`), bad: true},
		{name: "model 空串", edit: message("model", `""`)},
		{name: "timestamp 是数字", edit: record("timestamp", `123`), bad: true},
		{name: "timestamp 不是时间", edit: record("timestamp", `"bad"`), bad: true},
		{name: "timestamp 缺失", edit: func(r, _, _ obj) { delete(r, "timestamp") }, bad: true},
		{name: "timestamp 带偏移", edit: record("timestamp", `"2026-09-02T08:00:00+08:00"`), want: &base},
		{name: "effort 是数字", edit: record("effort", `1`), bad: true},
		{name: "effort 为 null", edit: record("effort", `null`), want: &base},
		{name: "不是 assistant", edit: record("type", `"user"`)},
		{name: "顶层是数组", line: `["usage", "assistant"]`, bad: true},
		{name: "JSON 不完整", line: `{"type": "assistant", "usage": 1`, bad: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := newTestContext(t)
			line := c.line
			if line == "" {
				line = claudeLine(t, c.edit)
			}
			src := sources["claude"].(*claudeSource)
			n, err := src.ingestLine(ctx, filepath.Join(t.TempDir(), "s.jsonl"), "s.jsonl", []byte(line), &fileState{})
			if err != nil && !isLineError(err) {
				t.Fatalf("不是行错误: %v", err)
			}
			if (err != nil) != c.bad {
				t.Fatalf("坏行 = %v（%v），期望 %v", err != nil, err, c.bad)
			}
			rows := mustQuery(t, ctx, "SELECT * FROM events")
			if c.want == nil {
				if n != 0 || len(rows) != 0 {
					t.Fatalf("入库 %d 条、表里 %d 行，期望不入库", n, len(rows))
				}
				return
			}
			if n != 1 || len(rows) != 1 {
				t.Fatalf("入库 %d 条、表里 %d 行，期望 1", n, len(rows))
			}
			r := rows[0]
			got := eventTokens{r.Int("input_tokens"), r.Int("cached_input_tokens"), r.Int("cache_write_5m_tokens"),
				r.Int("cache_write_1h_tokens"), r.Int("output_tokens"), r.Int("reasoning_tokens")}
			if got != *c.want {
				t.Errorf("计数 = %v，期望 %v", got, *c.want)
			}
		})
	}
}

// 额度读数：整数写成小数或指数、数字字符串、布尔都按数取；取不出数的行跳过并在 warning 里给出行号。
func TestReadingLineAcceptanceRules(t *testing.T) {
	ctx := newTestContext(t)
	lines := []string{
		`{"ts": 1790000001, "used_percentage": 31.0, "resets_at": 1790442000.0}`,
		`{"ts": 1790000002, "used_percentage": 31.0, "resets_at": 1.790442e9}`,
		`{"ts": "1790000003", "used_percentage": "31.5", "resets_at": 1790442000}`,
		`{"ts": 1790000004, "used_percentage": true, "resets_at": 1790442000.7}`,
		`{"ts": 1790000005, "used_percentage": 32, "resets_at": 1790442000, "dropped_from": null}`,
		`{"ts": 1790000006, "used_percentage": 33, "resets_at": 1790442000, "dropped_from": 40}`,
		`{"ts": 1790000007, "used_percentage": "abc", "resets_at": 1790442000}`,                   // 第 7 行
		`{"ts": 1790000008, "used_percentage": null, "resets_at": 1790442000}`,                    // 第 8 行
		`{"ts": 1790000009, "used_percentage": 34}`,                                               // 第 9 行
		`{"ts": 1790000010, "used_percentage": 35, "resets_at": 1790442000, "dropped_from": "x"}`, // 第 10 行
		`[1]`, // 第 11 行
	}
	writeFile(t, ctx.Args.rateLimitLog("claude"), strings.Join(lines, "\n")+"\n")
	if err := sources["claude"].CollectReadings(ctx); err != nil {
		t.Fatal(err)
	}
	type reading struct {
		Ts, Used    float64
		Resets      int64
		DroppedFrom *float64
	}
	var got []reading
	for _, r := range mustQuery(t, ctx, "SELECT * FROM readings ORDER BY ts") {
		got = append(got, reading{r.Float("ts"), r.Float("used_percent"), r.Int("resets_at"), r.FloatP("dropped_from")})
	}
	want := []reading{
		{1790000001, 31, 1790442000, nil},
		{1790000002, 31, 1790442000, nil},
		{1790000003, 31.5, 1790442000, nil},
		{1790000004, 1, 1790442000, nil},
		{1790000005, 32, 1790442000, nil},
		{1790000006, 33, 1790442000, floatp(40)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("读数 = %+v，期望 %+v", got, want)
	}
	if len(ctx.Warnings) != 1 || !strings.Contains(ctx.Warnings[0], "跳过 5 行") ||
		!strings.Contains(ctx.Warnings[0], "第 7, 8, 9, 10, 11 行") {
		t.Errorf("warnings = %q", ctx.Warnings)
	}
}

// 会话号不是字符串时，登记与会话日志两边按同一写法比对，仍能对上。
func TestNonStringSessionIDMatchesRegistration(t *testing.T) {
	ctx := newTestContext(t)
	writeFile(t, ctx.Args.sessionAccountLog(),
		`{"ts": 0, "session_id": 42, "account": "claude-b"}`+"\n"+
			`{"ts": 0, "session_id": null, "account": "claude-b"}`+"\n"+
			`{"ts": 0, "account": "claude-b"}`+"\n") // 第 3 行缺 session_id，跳过
	writeJSONL(t, filepath.Join(ctx.Args.ClaudeHome, "projects", "proj", "s.jsonl"),
		with(ingestClaudeMessage("m_num", 10), obj{"sessionId": 42}),
		with(ingestClaudeMessage("m_null", 10), obj{"sessionId": nil}),
	)
	mustSync(t, ctx, "claude")
	want := map[string]string{"m_num": "claude-b", "m_null": "claude"}
	if got := acctSources(t, ctx); !reflect.DeepEqual(got, want) {
		t.Errorf("归属 = %v，期望 %v", got, want)
	}
	if len(ctx.Warnings) != 1 || !strings.Contains(ctx.Warnings[0], "有 1 行无法解析") {
		t.Errorf("warnings = %q", ctx.Warnings)
	}
}

// 行错误写明字段与期望类型。
func TestLineErrorsNameFieldAndType(t *testing.T) {
	ctx := newTestContext(t)
	src := sources["claude"].(*claudeSource)
	for _, c := range []struct{ line, want string }{
		{claudeLine(t, func(_, _, u obj) { u["input_tokens"] = "5" }), "字段 input_tokens 应为数字，实际是字符串"},
		{claudeLine(t, func(r, _, _ obj) { delete(r, "timestamp") }), "缺少字段 timestamp"},
		{claudeLine(t, func(r, _, _ obj) { r["timestamp"] = "bad" }), `时间戳 "bad" 不是 ISO 8601 写法`},
		{claudeLine(t, func(_, m, _ obj) { m["id"] = 7 }), "字段 id 应为字符串，实际是数字"},
		{claudeLine(t, func(r, _, _ obj) { r["message"] = "usage" }), "字段 message 应为对象，实际是字符串"},
		{`["usage", "assistant"]`, "顶层应为对象，实际是数组"},
	} {
		_, err := src.ingestLine(ctx, filepath.Join(t.TempDir(), "s.jsonl"), "s.jsonl", []byte(c.line), &fileState{})
		if err == nil || !isLineError(err) || err.Error() != c.want {
			t.Errorf("%s: err = %v，期望行错误 %q", c.line, err, c.want)
		}
	}
	if _, err := requireNum(obj{"used_percentage": "abc"}, "used_percentage"); err == nil ||
		err.Error() != `字段 used_percentage 的值 "abc" 不是数字` {
		t.Errorf("requireNum err = %v", err)
	}
}
