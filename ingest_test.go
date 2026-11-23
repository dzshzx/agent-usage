// 各来源日志的入库规则：去重、子代理重放、坏行跳过。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ingestClaudeMessage 是 claudeMessage 取默认时间与模型。
func ingestClaudeMessage(msgID string, out int64) obj {
	return claudeMessage(msgID, out, "2026-09-02T00:00:00Z", "claude-opus-5")
}

func TestClaudeCountsEachMessageOnce(t *testing.T) {
	ctx := newTestContext(t)
	projects := filepath.Join(ctx.Args.ClaudeHome, "projects")
	writeJSONL(t, filepath.Join(projects, "proj", "session.jsonl"),
		ingestClaudeMessage("msg_1", 40),
		ingestClaudeMessage("msg_1", 90), // 同一条消息按内容块拆成多行
		claudeMessage("msg_2", 10, "2026-09-02T00:00:00Z", "<synthetic>"),
		obj{"type": "user", "message": obj{"role": "user"}},
	)
	writeJSONL(t, filepath.Join(projects, "proj", "session", "subagents", "agent-1.jsonl"), ingestClaudeMessage("msg_1", 90))
	mustSync(t, ctx, "claude")
	rows := mustQuery(t, ctx, "SELECT * FROM events WHERE source='claude'")
	if len(rows) != 1 {
		t.Fatalf("claude 请求 %d 行, want 1", len(rows))
	}
	row := rows[0]
	got := [5]int64{row.Int("input_tokens"), row.Int("cached_input_tokens"), row.Int("cache_write_5m_tokens"),
		row.Int("cache_write_1h_tokens"), row.Int("output_tokens")}
	if want := [5]int64{1302, 1000, 100, 200, 90}; got != want {
		t.Errorf("token 计数 = %v, want %v", got, want)
	}
	if row.IsNull("cost") || row.Str("limit_id") != "seven_day" {
		t.Errorf("cost=%v limit_id=%v, want 非空与 seven_day", row["cost"], row["limit_id"])
	}
}

// 同一消息已记在一个账号下时，另一个账号再写它既不多出一行，也不改原来那一行（输出更多也不改）。
func TestEventStaysWithItsAccount(t *testing.T) {
	ctx := newTestContext(t)
	syncAddEvent(t, ctx, "claude", "m", 60)
	more := syncEvent("m", 60)
	more.OutputTokens = 900
	if added, err := ctx.AddEvent(accountSource("claude-b"), more); err != nil || added {
		t.Errorf("另一个账号写同一消息: added=%v err=%v", added, err)
	}
	rows := mustQuery(t, ctx, "SELECT source, output_tokens FROM events")
	if len(rows) != 1 || rows[0].Str("source") != "claude" || rows[0].Int("output_tokens") != 10 {
		t.Errorf("库里 = %v", rows)
	}
}

// cache_creation 里没有 5 分钟分项时，5 分钟写入取总写入减去 1 小时部分。
func TestClaudeCacheWrite5mDefaultsToTotalMinus1h(t *testing.T) {
	ctx := newTestContext(t)
	msg := ingestClaudeMessage("msg_1", 10)
	msg["message"].(obj)["usage"].(obj)["cache_creation"] = obj{"ephemeral_1h_input_tokens": 200}
	writeJSONL(t, filepath.Join(ctx.Args.ClaudeHome, "projects", "proj", "session.jsonl"), msg)
	mustSync(t, ctx, "claude")
	rows := mustQuery(t, ctx, "SELECT * FROM events WHERE source='claude'")
	if len(rows) != 1 {
		t.Fatalf("claude 请求 %d 行, want 1", len(rows))
	}
	row := rows[0]
	got := [3]int64{row.Int("input_tokens"), row.Int("cache_write_5m_tokens"), row.Int("cache_write_1h_tokens")}
	if want := [3]int64{1302, 100, 200}; got != want {
		t.Errorf("input、5m、1h = %v, want %v", got, want)
	}
}

func TestClaudeReadingsFromReadingLog(t *testing.T) {
	ctx := newTestContext(t)
	var b strings.Builder
	for _, r := range []obj{
		{"ts": 1790000100, "window": "seven_day", "used_percentage": 31.0, "resets_at": 1790442000},
		{"ts": 1790000200, "window": "seven_day", "used_percentage": 32.0, "resets_at": 1790442000},
	} {
		line, _ := json.Marshal(r)
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteString("not json\n")
	writeFile(t, ctx.Args.rateLimitLog("claude"), b.String())
	for range 2 {
		if err := sources["claude"].CollectReadings(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := count(t, ctx, "readings", "source='claude' AND limit_id='seven_day'"); got != 2 {
		t.Errorf("readings = %d, want 2", got)
	}
}

func TestBadLinesAreSkippedWithLocationAndProgressKept(t *testing.T) {
	ctx := newTestContext(t)
	claudeLog := writeJSONL(t, filepath.Join(ctx.Args.ClaudeHome, "projects", "proj", "s.jsonl"),
		ingestClaudeMessage("msg_ok1", 10),
		without(ingestClaudeMessage("msg_bad", 10), "timestamp"),                 // 第 2 行：缺 timestamp
		with(ingestClaudeMessage("msg_odd", 10), obj{"message": []any{"usage"}}), // 第 3 行：结构异常
		ingestClaudeMessage("msg_ok2", 10),
	)
	writeFile(t, ctx.Args.rateLimitLog("claude"),
		`{"ts": 1790000100, "used_percentage": 31.0}`+"\n"+
			`{"ts": 1790000200, "used_percentage": 32.0, "resets_at": 1790442000}`+"\n")
	syncAll := func() {
		t.Helper()
		mustSync(t, ctx, "claude")
		if err := sources["claude"].CollectReadings(ctx); err != nil {
			t.Fatal(err)
		}
	}
	syncAll()

	if got := count(t, ctx, "events", "source='claude'"); got != 2 {
		t.Errorf("claude events = %d, want 2", got)
	}
	if got := count(t, ctx, "readings", "source='claude'"); got != 1 {
		t.Errorf("claude readings = %d, want 1", got)
	}
	warns := ctx.Warnings
	if len(warns) != 2 {
		t.Fatalf("warnings = %q, want 2 条", warns)
	}
	if !strings.Contains(warns[0], claudeLog+" 第 2, 3 行") || !strings.Contains(warns[0], "timestamp") {
		t.Errorf("warns[0] = %q", warns[0])
	}
	if !strings.Contains(warns[1], ctx.Args.rateLimitLog("claude")+" 第 1 行") {
		t.Errorf("warns[1] = %q", warns[1])
	}
	// 进度越过坏行并已提交：再跑一次不重复报、不重复计
	ctx.Warnings = nil
	syncAll()
	if len(ctx.Warnings) != 0 {
		t.Errorf("再次同步仍有警告: %q", ctx.Warnings)
	}
	if got := count(t, ctx, "events", "source='claude'"); got != 2 {
		t.Errorf("再次同步后 claude events = %d, want 2", got)
	}
	info, err := os.Stat(claudeLog)
	if err != nil {
		t.Fatal(err)
	}
	offsets := mustQuery(t, ctx, "SELECT name, offset FROM files WHERE source='claude' AND name='proj/s.jsonl'")
	if len(offsets) != 1 || offsets[0].Int("offset") != info.Size() {
		t.Errorf("会话日志进度 = %v, want 一行、offset %d", offsets, info.Size())
	}
}

// ingestAgentEffort 是一行请求的 (键, agent, effort)。
type ingestAgentEffort struct{ key, agent, effort string }

func ingestAgentEffortRows(t *testing.T, ctx *Context, query string) []ingestAgentEffort {
	t.Helper()
	var out []ingestAgentEffort
	for _, r := range mustQuery(t, ctx, query) {
		out = append(out, ingestAgentEffort{r.Str("k"), r.Str("agent"), r.Str("effort")})
	}
	return out
}

func TestClaudeAgentAndEffort(t *testing.T) {
	ctx := newTestContext(t)
	projects := filepath.Join(ctx.Args.ClaudeHome, "projects", "proj")
	writeJSONL(t, filepath.Join(projects, "s.jsonl"), with(ingestClaudeMessage("msg_main", 10), obj{"effort": "xhigh"}))
	sub := filepath.Join(projects, "s", "subagents", "agent-x.jsonl")
	writeJSONL(t, sub, with(ingestClaudeMessage("msg_sub", 10), obj{"isSidechain": true, "effort": "high"}))
	writeFile(t, filepath.Join(filepath.Dir(sub), "agent-x.meta.json"), `{"agentType": "reviewer"}`)
	mustSync(t, ctx, "claude")

	got := ingestAgentEffortRows(t, ctx, "SELECT ukey k, agent, effort FROM events WHERE source='claude' ORDER BY ukey")
	if want := []ingestAgentEffort{{"msg_main", "main", "xhigh"}, {"msg_sub", "reviewer", "high"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// 子代理元数据是合法 JSON 但顶层不是对象时按没有元数据处理：该子代理日志的请求全部照常计入，记作 subagent，不报坏行。
func TestSubagentMetaThatIsNotAnObjectCountsAsNoMeta(t *testing.T) {
	for _, meta := range []string{`null`, `[]`, `[{"agentType": "reviewer"}]`, `"reviewer"`, `5`, `true`, `{}`, `not json`} {
		t.Run(meta, func(t *testing.T) {
			ctx := newTestContext(t)
			sub := filepath.Join(ctx.Args.ClaudeHome, "projects", "proj", "s", "subagents", "agent-x.jsonl")
			writeJSONL(t, sub,
				with(ingestClaudeMessage("msg_sub_1", 10), obj{"isSidechain": true}),
				with(ingestClaudeMessage("msg_sub_2", 20), obj{"isSidechain": true}))
			writeFile(t, filepath.Join(filepath.Dir(sub), "agent-x.meta.json"), meta)
			mustSync(t, ctx, "claude")

			got := ingestAgentEffortRows(t, ctx, "SELECT ukey k, agent, '' effort FROM events WHERE source='claude' ORDER BY ukey")
			if want := []ingestAgentEffort{{"msg_sub_1", "subagent", ""}, {"msg_sub_2", "subagent", ""}}; !reflect.DeepEqual(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
			if len(ctx.Warnings) != 0 {
				t.Errorf("warnings = %q, want 无", ctx.Warnings)
			}
		})
	}
}

// 缺 addedColumns 里的列的库打开时补上这一列，已有的行原样保留；再开一次不重复补。
func TestMissingAddedColumnIsAppended(t *testing.T) {
	dir := t.TempDir()
	old, err := openSQLite(filepath.Join(dir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := old.ExecScript(schema + "ALTER TABLE prices DROP COLUMN long_cache_write_1h;" +
		"INSERT INTO prices (source, model, tier, valid_from, input, output, url) VALUES ('claude', 'm', 'standard', 0, 1, 2, 'u');"); err != nil {
		t.Fatal(err)
	}
	old.Close()
	for range 2 {
		db, err := openDB(dir)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := db.Query("SELECT model, input, long_cache_write_1h FROM prices")
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Str("model") != "m" || rows[0].Float("input") != 1 || !rows[0].IsNull("long_cache_write_1h") {
			t.Fatalf("补列后的行 = %v，期望原有一行、新列为空", rows)
		}
	}
}

func TestEmptyEnvVarFallsBackToDefault(t *testing.T) {
	def := t.TempDir()
	for _, c := range []struct{ value, want string }{
		{"", def},
		{"/x", "/x"},
	} {
		t.Run("CLAUDE_CONFIG_DIR="+c.value, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", c.value)
			if got := envPath("CLAUDE_CONFIG_DIR", def); got != c.want {
				t.Errorf("envPath = %q, want %q", got, c.want)
			}
		})
	}
}
