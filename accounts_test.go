// Claude Code 两个账号（claude 与 claude-b）按会话登记拆分用量与读数。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// acctMessage 是带会话号的 Claude 消息。
func acctMessage(msgID, session, ts string, out int64, sidechain bool) obj {
	return with(claudeMessage(msgID, out, ts, "claude-opus-5"), obj{"sessionId": session, "isSidechain": sidechain})
}

func acctMsg(msgID, session, ts string) obj { return acctMessage(msgID, session, ts, 10, false) }

// acctReg 是一条会话账号登记。
func acctReg(t *testing.T, ts, session, account string) obj {
	return obj{"ts": mustParseTs(t, ts), "session_id": session, "account": account}
}

// acctTarget 是会话切到 claude-switch 非账号目标的登记（hook 写 "target": true）。
func acctTarget(t *testing.T, ts, session string) obj {
	return obj{"ts": mustParseTs(t, ts), "session_id": session, "account": "gateway", "target": true}
}

func acctRegister(t *testing.T, ctx *Context, rows ...obj) {
	t.Helper()
	writeJSONL(t, ctx.Args.sessionAccountLog(), acctAny(rows)...)
}

func acctAny(rows []obj) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}

// acctResumeRegs：旧会话 old 登记在 claude-b，resume 出的新会话 new 登记在 claude。
func acctResumeRegs(t *testing.T) []obj {
	return []obj{
		acctReg(t, "2026-10-17T00:00:00Z", "old", "claude-b"),
		acctReg(t, "2026-10-17T05:00:00Z", "new", "claude"),
	}
}

// acctFile 是 projects/proj 下的一个会话文件；mtime 非 0 时设成该 Unix 秒。
type acctFile struct {
	name    string
	records []obj
	mtime   int64
}

func acctWriteFiles(t *testing.T, ctx *Context, files []acctFile) {
	t.Helper()
	projects := filepath.Join(ctx.Args.ClaudeHome, "projects", "proj")
	for _, f := range files {
		path := writeJSONL(t, filepath.Join(projects, f.name), acctAny(f.records)...)
		if f.mtime != 0 {
			tm := time.Unix(f.mtime, 0)
			if err := os.Chtimes(path, tm, tm); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// acctCopyBeforeOriginal：按 acctResumeRegs 登记；复制件 new.jsonl 的 mtime 更早，先被扫到。
// copy 为 nil 时写一条复制消息 m_copy；original 为 nil 时不写旧会话文件。
func acctCopyBeforeOriginal(t *testing.T, ctx *Context, copy, original []obj) {
	t.Helper()
	if copy == nil {
		copy = []obj{acctMsg("m_copy", "new", "2026-10-17T01:00:00Z")}
	}
	acctRegister(t, ctx, acctResumeRegs(t)...)
	files := []acctFile{{"new.jsonl", copy, 1000}}
	if original != nil {
		files = append(files, acctFile{"old.jsonl", original, 2000})
	}
	acctWriteFiles(t, ctx, files)
}

// acctSources 返回 ukey → 记在哪个来源下。
func acctSources(t *testing.T, ctx *Context) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, r := range mustQuery(t, ctx, "SELECT ukey, source FROM events ORDER BY ukey") {
		out[r.Str("ukey")] = r.Str("source")
	}
	return out
}

func acctOutputTokens(t *testing.T, ctx *Context, ukey string) int64 {
	t.Helper()
	rows := mustQuery(t, ctx, "SELECT output_tokens FROM events WHERE ukey=?", ukey)
	if len(rows) == 0 {
		t.Fatalf("找不到 %s", ukey)
	}
	return rows[0].Int("output_tokens")
}

// TestSessionAccountAttribution 覆盖登记与归属的各场景：每个场景写登记与会话文件、同步一次 claude，比对各消息的归属。
func TestSessionAccountAttribution(t *testing.T) {
	cases := []struct {
		name  string
		regs  func(t *testing.T) []obj
		files func() []acctFile
		// copyBeforeOriginal 为真时改用 acctCopyBeforeOriginal 布置（copy/original 取 files 里的 new/old）
		copyBeforeOriginal bool
		want               map[string]string
		check              func(t *testing.T, ctx *Context)
	}{
		{
			name: "without_registry_everything_is_claude",
			files: func() []acctFile {
				return []acctFile{{"s1.jsonl", []obj{acctMsg("m1", "s1", "2026-10-17T01:00:00Z")}, 0}}
			},
			want: map[string]string{"m1": "claude"},
		},
		{
			// 非账号目标那段走 API Key：不进报表；同一会话 resume 换过号的，只计订阅账号那段
			name: "target_sessions_are_not_counted",
			regs: func(t *testing.T) []obj {
				return []obj{
					acctTarget(t, "2026-10-17T00:00:00Z", "sc"),
					acctReg(t, "2026-10-17T00:00:00Z", "mix", "claude-b"),
					acctTarget(t, "2026-10-17T02:00:00Z", "mix"),
				}
			},
			files: func() []acctFile {
				return []acctFile{
					{"sc.jsonl", []obj{acctMsg("m_target", "sc", "2026-10-17T01:00:00Z")}, 0},
					{"sc/subagents/agent-1.jsonl", []obj{acctMessage("m_target_sub", "sc", "2026-10-17T01:05:00Z", 10, true)}, 0},
					{"mix.jsonl", []obj{
						acctMsg("m_b", "mix", "2026-10-17T01:00:00Z"),
						acctMsg("m_mix_target", "mix", "2026-10-17T03:00:00Z"),
					}, 0},
				}
			},
			want: map[string]string{"m_b": "claude-b", "m_target": targetSource, "m_target_sub": targetSource, "m_mix_target": targetSource},
			check: func(t *testing.T, ctx *Context) {
				if got, want := ctx.claudeSync.added, map[string]int{"claude-b": 1, targetSource: 3}; !reflect.DeepEqual(got, want) {
					t.Errorf("新增计数 = %v, want %v", got, want)
				}
				if _, ok := sources[targetSource]; ok {
					t.Error("非账号目标不应是可报告的来源")
				}
			},
		},
		{
			// 非账号目标的会话 resume（新会话仍是目标或已换回订阅账号）：复制件先被扫到，原件在目标下，复制件也只算目标
			name: "target_resume_copies_stay_target",
			regs: func(t *testing.T) []obj {
				return []obj{
					acctTarget(t, "2026-10-17T00:00:00Z", "old"),
					acctTarget(t, "2026-10-17T05:00:00Z", "new"),
					acctTarget(t, "2026-10-17T00:00:00Z", "old2"),
					acctReg(t, "2026-10-17T05:00:00Z", "new2", "claude"),
				}
			},
			files: func() []acctFile {
				return []acctFile{
					{"new.jsonl", []obj{acctMsg("m_old", "new", "2026-10-17T01:00:00Z")}, 1000},
					{"new2.jsonl", []obj{acctMsg("m_old2", "new2", "2026-10-17T01:00:00Z")}, 1000},
					{"old.jsonl", []obj{acctMsg("m_old", "old", "2026-10-17T01:00:00Z")}, 2000},
					{"old2.jsonl", []obj{acctMsg("m_old2", "old2", "2026-10-17T01:00:00Z")}, 2000},
				}
			},
			want: map[string]string{"m_old": targetSource, "m_old2": targetSource},
			check: func(t *testing.T, ctx *Context) {
				if rows := mustQuery(t, ctx, "SELECT ukey FROM events WHERE source != ?", targetSource); len(rows) != 0 {
					t.Errorf("复制件被计入订阅账号：%d 行", len(rows))
				}
			},
		},
		{
			// 登记里的任何账号名都按它自己的账号入库（claude-switch add 加的号）；不合账号名写法的登记忽略，会话归主号
			name: "registered_sessions_and_subagents_go_to_their_accounts",
			regs: func(t *testing.T) []obj {
				return []obj{
					acctReg(t, "2026-10-17T00:00:00Z", "sb", "claude-b"),
					acctReg(t, "2026-10-17T00:00:00Z", "sa", "claude"),
					acctReg(t, "2026-10-17T00:00:00Z", "sx", "work"),
					acctReg(t, "2026-10-17T00:00:00Z", "sy", "Not An Account"),
				}
			},
			files: func() []acctFile {
				return []acctFile{
					{"sb.jsonl", []obj{acctMsg("m_main", "sb", "2026-10-17T01:00:00Z")}, 0},
					{"sb/subagents/agent-1.jsonl", []obj{acctMessage("m_sub", "sb", "2026-10-17T01:05:00Z", 10, true)}, 0},
					{"sa.jsonl", []obj{acctMsg("m_a", "sa", "2026-10-17T01:00:00Z")}, 0},
					{"sx.jsonl", []obj{acctMsg("m_x", "sx", "2026-10-17T01:00:00Z")}, 0},
					{"sy.jsonl", []obj{acctMsg("m_y", "sy", "2026-10-17T01:00:00Z")}, 0},
				}
			},
			want: map[string]string{"m_main": "claude-b", "m_sub": "claude-b", "m_a": "claude", "m_x": "work", "m_y": "claude"},
			check: func(t *testing.T, ctx *Context) {
				rows := mustQuery(t, ctx, "SELECT cost FROM events WHERE ukey='m_main'")
				if len(rows) != 1 || rows[0].IsNull("cost") { // 沿用 claude 的价格表
					t.Errorf("m_main cost 为空: %v", rows)
				}
			},
		},
		{
			name: "resume_switch_splits_session_by_time",
			regs: func(t *testing.T) []obj {
				return []obj{
					acctReg(t, "2026-10-17T00:00:00Z", "s", "claude"),
					acctReg(t, "2026-10-17T02:00:00Z", "s", "claude-b"),
				}
			},
			files: func() []acctFile {
				return []acctFile{{"s.jsonl", []obj{
					acctMsg("m_early", "s", "2026-10-17T01:00:00Z"),
					acctMsg("m_late", "s", "2026-10-17T03:00:00Z"),
				}, 0}}
			},
			want: map[string]string{"m_early": "claude", "m_late": "claude-b"},
		},
		{
			name: "registration_at_message_time_applies",
			regs: func(t *testing.T) []obj { return []obj{acctReg(t, "2026-10-17T01:00:00Z", "s", "claude-b")} },
			files: func() []acctFile {
				return []acctFile{{"s.jsonl", []obj{acctMsg("m", "s", "2026-10-17T01:00:00Z")}, 0}}
			},
			want: map[string]string{"m": "claude-b"},
		},
		{
			name:               "resume_copy_scanned_before_original_keeps_original_account",
			copyBeforeOriginal: true,
			files: func() []acctFile {
				return []acctFile{
					{"new.jsonl", []obj{
						acctMessage("m_old", "new", "2026-10-17T01:00:00Z", 20, false),
						acctMsg("m_new", "new", "2026-10-17T06:00:00Z"),
					}, 0},
					{"old.jsonl", []obj{acctMsg("m_old", "old", "2026-10-17T01:00:00Z")}, 0},
				}
			},
			want: map[string]string{"m_new": "claude", "m_old": "claude-b"},
			check: func(t *testing.T, ctx *Context) {
				if got := count(t, ctx, "events", "ukey='m_old'"); got != 1 {
					t.Errorf("m_old 行数 = %d, want 1", got)
				}
				if got := acctOutputTokens(t, ctx, "m_old"); got != 20 {
					t.Errorf("m_old output_tokens = %d, want 20", got)
				}
			},
		},
		{
			name:               "copy_without_original_goes_to_claude",
			copyBeforeOriginal: true,
			want:               map[string]string{"m_copy": "claude"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := newTestContext(t)
			acctArrange(t, ctx, c.regs, c.files, c.copyBeforeOriginal)
			mustSync(t, ctx, "claude")
			if got := acctSources(t, ctx); !reflect.DeepEqual(got, c.want) {
				t.Errorf("归属 = %v, want %v", got, c.want)
			}
			if c.check != nil {
				c.check(t, ctx)
			}
		})
	}
}

// acctArrange 布置一个场景的登记与会话文件。copyBeforeOriginal 为真时改用 acctCopyBeforeOriginal 布置
// （copy/original 取 filesOf 里的 new/old），否则写 regs 给出的登记与 filesOf 给出的会话文件；regs、filesOf 可为 nil。
func acctArrange(t *testing.T, ctx *Context, regs func(t *testing.T) []obj, filesOf func() []acctFile, copyBeforeOriginal bool) {
	var files []acctFile
	if filesOf != nil {
		files = filesOf()
	}
	if copyBeforeOriginal {
		var copy, original []obj
		for _, f := range files {
			switch f.name {
			case "new.jsonl":
				copy = f.records
			case "old.jsonl":
				original = f.records
			}
		}
		acctCopyBeforeOriginal(t, ctx, copy, original)
	} else {
		if regs != nil {
			acctRegister(t, ctx, regs(t)...)
		}
		acctWriteFiles(t, ctx, files)
	}
}

func TestResumeCopiedMessagesAreNotDoubleCounted(t *testing.T) {
	ctx := newTestContext(t)
	acctRegister(t, ctx, acctResumeRegs(t)...)
	acctWriteFiles(t, ctx, []acctFile{{"old.jsonl", []obj{acctMsg("m_old", "old", "2026-10-17T01:00:00Z")}, 0}})
	mustSync(t, ctx, "claude")
	// resume 把旧消息复制进新会话文件：sessionId 换成新会话，时间仍是旧的
	acctWriteFiles(t, ctx, []acctFile{{"new.jsonl", []obj{
		acctMessage("m_old", "new", "2026-10-17T01:00:00Z", 20, false),
		acctMsg("m_new", "new", "2026-10-17T06:00:00Z"),
	}, 0}})
	mustSync(t, ctx, "claude")
	if got, want := acctSources(t, ctx), map[string]string{"m_new": "claude", "m_old": "claude-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("归属 = %v, want %v", got, want)
	}
	if got := count(t, ctx, "events", "ukey='m_old'"); got != 1 {
		t.Errorf("m_old 行数 = %d, want 1", got)
	}
	if got := acctOutputTokens(t, ctx, "m_old"); got != 20 {
		t.Errorf("m_old output_tokens = %d, want 20", got)
	}
}

func TestClaudeBReadingsFromOwnFile(t *testing.T) {
	ctx := newTestContext(t)
	line := obj{"ts": 1790000100, "window": "seven_day", "used_percentage": 12.0, "resets_at": 1790442000}
	writeJSONL(t, ctx.Args.rateLimitLog("claude-b"), line)
	writeJSONL(t, ctx.Args.rateLimitLog("claude"), with(line, obj{"used_percentage": 40.0}))
	for _, name := range []string{"claude-b", "claude"} {
		if err := accountSource(name).CollectReadings(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		where string
		want  int64
	}{
		{"source='claude-b' AND used_percent=12", 1},
		{"source='claude-b'", 1},
		{"source='claude' AND used_percent=40", 1},
		{"source='claude'", 1},
	} {
		if got := count(t, ctx, "readings", c.where); got != c.want {
			t.Errorf("readings %s = %d, want %d", c.where, got, c.want)
		}
	}
}

func TestClaudeBDoesNotFetchPrices(t *testing.T) {
	ctx := newTestContext(t)
	orig := fetchText
	fetchText = func(url string) (string, error) {
		t.Errorf("claude-b 不应抓取价格页: %s", url)
		return "", errors.New("claude-b 不应抓取价格页")
	}
	t.Cleanup(func() { fetchText = orig })
	ctx.Args = &Args{Offline: false, RefreshPrices: false, MaxPriceAge: 7.0}
	before := count(t, ctx, "price_checks", "")
	refreshed, err := ctx.ensurePrices(accountSource("claude-b"), false)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Error("ensurePrices 返回 true, want false")
	}
	if got := count(t, ctx, "price_checks", ""); got != before {
		t.Errorf("price_checks = %d, want %d", got, before)
	}
	state, err := priceState(ctx.DB, accountSource("claude-b"))
	if err != nil {
		t.Fatal(err)
	}
	if state.LastAlignedAt == nil {
		t.Error("LastAlignedAt 为空")
	}
}

func TestClaudeBSyncReusesClaudeScan(t *testing.T) {
	ctx := newTestContext(t)
	acctRegister(t, ctx, acctReg(t, "2026-10-17T00:00:00Z", "s", "claude-b"))
	acctWriteFiles(t, ctx, []acctFile{{"s.jsonl", []obj{acctMsg("m", "s", "2026-10-17T01:00:00Z")}, 0}})
	claude := sources["claude"].(*claudeSource)
	before := claude.scans
	first := mustSync(t, ctx, "claude-b") // 单独选 claude-b 时由 claude 扫一遍
	if claude.scans-before != 1 || first.EventsAdded != 1 {
		t.Errorf("扫描 %d 次、EventsAdded=%d, want 1 与 1", claude.scans-before, first.EventsAdded)
	}
	mustSync(t, ctx, "claude")
	before = claude.scans
	if got := mustSync(t, ctx, "claude-b").EventsAdded; got != 0 {
		t.Errorf("再次 claude-b EventsAdded = %d, want 0", got)
	}
	if claude.scans != before {
		t.Errorf("claude-b 又扫了 %d 次, want 0", claude.scans-before)
	}
}

func TestIngestLineWithoutSyncDoesNotSwallow(t *testing.T) {
	ctx := newTestContext(t)
	line, err := json.Marshal(acctMsg("m", "s", "2026-10-17T01:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	src := sources["claude"].(*claudeSource)
	n, err := src.ingestLine(ctx, filepath.Join(t.TempDir(), "s.jsonl"), "s.jsonl", line, &fileState{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("ingestLine = %d, want 1", n)
	}
}

func TestMainUsesSharedPriceTableForClaudeB(t *testing.T) {
	ctx := newTestContext(t)
	db := ctx.DB
	for _, q := range []string{
		"UPDATE price_checks SET checked_at = checked_at - 7200",
		// claude-b 里有个对齐之前就入库的未知模型：不应触发提前对齐
		"INSERT INTO events (source, ukey, ts, file, model, tier, input_tokens, cached_input_tokens, " +
			"cache_write_5m_tokens, cache_write_1h_tokens, output_tokens, reasoning_tokens, limit_id) " +
			"VALUES ('claude-b', 'u', 1000, 'f', 'mystery-1', 'standard', 1, 0, 0, 0, 1, 0, 'seven_day')",
		"PRAGMA user_version=1",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var fetched []string
	orig := fetchText
	fetchText = func(url string) (string, error) { fetched = append(fetched, url); return "", nil }
	t.Cleanup(func() { fetchText = orig })
	t.Setenv("AGENT_USAGE_DATA_DIR", ctx.Args.DataDir)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"prices", "--source", "claude-b", "--no-sync"}, &stdout, &stderr); code != 0 {
		t.Fatalf("退出码 %d, stderr: %s", code, stderr.String())
	}
	var out struct {
		Sources map[string]struct {
			Prices struct {
				LastAlignedAt *string `json:"last_aligned_at"`
			} `json:"prices"`
			PricesTable []any `json:"prices_table"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("输出不是 JSON: %v\n%s", err, stdout.String())
	}
	block := out.Sources["claude-b"]
	if len(fetched) != 0 {
		t.Errorf("抓取了价格页: %v", fetched)
	}
	if block.Prices.LastAlignedAt == nil {
		t.Error("last_aligned_at 为空")
	}
	if len(block.PricesTable) == 0 {
		t.Errorf("prices_table 为空: %s", strings.TrimSpace(stdout.String()))
	}
}

func TestDeferredCopySurvivesFailureDuringFinish(t *testing.T) {
	ctx := newTestContext(t)
	acctCopyBeforeOriginal(t, ctx, nil, nil)
	testHookAddEvent = func(ev *Event) error {
		if ev.Ukey == "m_copy" {
			return errors.New("database is locked")
		}
		return nil
	}
	t.Cleanup(func() { testHookAddEvent = nil })
	if _, err := sources["claude"].Sync(ctx); err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("Sync err = %v, want database is locked", err)
	}
	testHookAddEvent = nil
	mustSync(t, ctx, "claude")
	if got, want := acctSources(t, ctx), map[string]string{"m_copy": "claude"}; !reflect.DeepEqual(got, want) {
		t.Errorf("归属 = %v, want %v", got, want)
	}
}

// 报表列出的账号：主号、读数目录里每个 rate-limits.<账号>.jsonl、库里有 7 天窗口数据的来源；
// 非账号目标与旧库里别的限额名（Codex）的来源不列。
func TestReportListsDiscoveredAccounts(t *testing.T) {
	ctx := newTestContext(t)
	writeJSONL(t, ctx.Args.rateLimitLog("work"), obj{"ts": 1790000100, "window": "seven_day", "used_percentage": 5.0, "resets_at": 1790442000})
	syncAddEvent(t, ctx, "claude-b", "m_b", 7200)
	syncAddEvent(t, ctx, targetSource, "m_target", 7200)
	mustAddEvent(t, ctx, accountSource("codex"), testEvent("m_codex", now()-7200, "f", "gpt", "codex", 10))
	out := cliRun(t, ctx, "history", "--offline")
	if got := strings.Join(slices.Sorted(maps.Keys(cliSources(t, out))), ","); got != "claude,claude-b,work" {
		t.Errorf("来源 = %s，want claude,claude-b,work", got)
	}
}
