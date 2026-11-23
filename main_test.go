// 命令行入口 run：各子命令的输出块、警告与价格对齐的触发条件。
package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cliRun 在 ctx 的库与目录上跑一次 run，返回解析后的 JSON 输出。测试自己的连接先关掉（已关过的跳过），可以连着跑几条命令。
func cliRun(t *testing.T, ctx *Context, argv ...string) map[string]any {
	t.Helper()
	if err := ctx.DB.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
	full := append(append([]string{}, argv...), cliPathArgs(ctx)...)
	var out, errBuf bytes.Buffer
	if code := run(full, &out, &errBuf); code != 0 {
		t.Fatalf("run 返回 %d，stderr: %s", code, errBuf.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("输出不是 JSON: %v\n%s", err, out.String())
	}
	return parsed
}

// cliPathArgs 是把 run 指到 ctx 的库与各目录的参数；加了新的路径参数时只改这里。
func cliPathArgs(ctx *Context) []string {
	a := ctx.Args
	return []string{"--data-dir", a.DataDir, "--claude-home", a.ClaudeHome,
		"--claude-switch-dir", a.SwitchStateDir, "--config", a.ConfigPath}
}

// cliWriteSession 在 Claude 项目目录下写一个只含一条消息的会话日志。
func cliWriteSession(t *testing.T, ctx *Context, model string) {
	t.Helper()
	path := filepath.Join(ctx.Args.ClaudeHome, "projects", "proj", "s.jsonl")
	writeJSONL(t, path, claudeMessage("m1", 10, "2026-09-02T00:00:00Z", model))
}

func cliMustExec(t *testing.T, ctx *Context, query string) {
	t.Helper()
	if _, err := ctx.DB.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func cliSources(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	s, ok := out["sources"].(map[string]any)
	if !ok {
		t.Fatalf("sources 不是对象: %#v", out["sources"])
	}
	return s
}

func cliWarnings(t *testing.T, out map[string]any) []string {
	t.Helper()
	raw, ok := out["warnings"].([]any)
	if !ok {
		t.Fatalf("warnings 不是数组: %#v", out["warnings"])
	}
	ws := make([]string, 0, len(raw))
	for _, w := range raw {
		ws = append(ws, w.(string))
	}
	return ws
}

func TestStatusSyncsAndWarnsWithoutActiveCycle(t *testing.T) {
	ctx := newTestContext(t)
	cliWriteSession(t, ctx, "claude-opus-5")
	out := cliRun(t, ctx, "status", "--offline", "--source", "claude")
	srcs := cliSources(t, out)
	if len(srcs) != 1 {
		t.Fatalf("sources 应只有 claude: %v", srcs)
	}
	block, ok := srcs["claude"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 claude 块: %v", srcs)
	}
	if block["currency"] != sources["claude"].Currency() {
		t.Errorf("currency = %v，期望 %v", block["currency"], sources["claude"].Currency())
	}
	if _, ok := block["sync"]; !ok {
		t.Error("缺少 sync")
	}
	if cycles, ok := block["cycles"].([]any); !ok || len(cycles) != 0 {
		t.Errorf("cycles 应为空数组: %#v", block["cycles"])
	}
	if _, ok := block["quota_reference"]; !ok {
		t.Error("缺少 quota_reference")
	}
	prices := block["prices"].(map[string]any)
	if prices["refreshed_now"] != false {
		t.Errorf("refreshed_now = %v，期望 false", prices["refreshed_now"])
	}
	want := "claude: 当前没有进行中的周期（本周期内尚无读数）"
	if ws := cliWarnings(t, out); !slices.Contains(ws, want) {
		t.Errorf("warnings %q 缺少 %q", ws, want)
	}
}

func TestHistoryListsUnpricedModels(t *testing.T) {
	ctx := newTestContext(t)
	cliWriteSession(t, ctx, "mystery-9")
	out := cliRun(t, ctx, "history", "--offline", "--source", "claude")
	block := cliSources(t, out)["claude"].(map[string]any)
	if cycles, ok := block["cycles"].([]any); !ok || len(cycles) != 1 {
		t.Errorf("cycles 应有 1 个: %#v", block["cycles"])
	}
	want := []string{"claude: 以下模型在价格表中找不到，其请求未计入金额: mystery-9"}
	if ws := cliWarnings(t, out); !slices.Equal(ws, want) {
		t.Errorf("warnings = %q，期望 %q", ws, want)
	}
}

func TestDailyAndPricesBlocks(t *testing.T) {
	cases := []struct {
		command string
		key     string
	}{
		{"daily", "daily"},
		{"prices", "prices_table"},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			ctx := newTestContext(t)
			out := cliRun(t, ctx, tc.command, "--offline", "--no-sync")
			srcs := cliSources(t, out)
			var got []string
			for name := range srcs {
				got = append(got, name)
			}
			slices.Sort(got)
			if !slices.Equal(got, sourceNames()) {
				t.Fatalf("sources = %v，期望 %v", got, sourceNames())
			}
			for name, raw := range srcs {
				block := raw.(map[string]any)
				if _, ok := block[tc.key]; !ok {
					t.Errorf("%s 缺少 %s", name, tc.key)
				}
				for _, absent := range []string{"cycles", "sync"} {
					if _, ok := block[absent]; ok {
						t.Errorf("%s 不应有 %s", name, absent)
					}
				}
			}
		})
	}
}

func TestNoPricesWarns(t *testing.T) {
	ctx := newTestContext(t)
	cliMustExec(t, ctx, "DELETE FROM price_checks")
	out := cliRun(t, ctx, "prices", "--offline", "--no-sync", "--source", "claude")
	prices := cliSources(t, out)["claude"].(map[string]any)["prices"].(map[string]any)
	if v, ok := prices["last_aligned_at"]; !ok || v != nil {
		t.Errorf("last_aligned_at 应为 null: %#v (存在=%v)", v, ok)
	}
	if !slices.ContainsFunc(cliWarnings(t, out), func(w string) bool {
		return strings.HasPrefix(w, "claude: 尚无已采集价格")
	}) {
		t.Errorf("warnings 缺少「claude: 尚无已采集价格」: %q", cliWarnings(t, out))
	}
}

// 价格页连不上、库里从没对齐过价格时：一次运行里只抓一次（开头为新库拉的那次之后，报表不再抓）；
// 一小时内的下一次运行也不再抓，不让每次运行都等一次超时；加 --refresh-prices 立即重试，也只抓一次。
func TestPriceFetchFailureNotRetried(t *testing.T) {
	ctx := newTestContext(t)
	cliMustExec(t, ctx, "DELETE FROM price_checks")
	fetched := 0
	orig := fetchText
	fetchText = func(string) (string, error) { fetched++; return "", errors.New("connection timed out") }
	defer func() { fetchText = orig }()
	for i, c := range []struct {
		argv []string
		want int
	}{
		{[]string{"status", "--no-sync"}, 1},
		{[]string{"status", "--no-sync"}, 1},
		{[]string{"status", "--no-sync", "--refresh-prices"}, 2},
	} {
		cliRun(t, ctx, c.argv...)
		if fetched != c.want {
			t.Errorf("第 %d 次运行后共抓了 %d 次，want %d", i+1, fetched, c.want)
		}
	}
}

func TestPricingRuleChangeRepricesAndBumpsVersion(t *testing.T) {
	ctx := newTestContext(t)
	cliWriteSession(t, ctx, "claude-opus-5")
	mustSync(t, ctx, "claude")
	cliMustExec(t, ctx, "UPDATE events SET cost = -1")
	if err := ctx.DB.setUserVersion(0); err != nil {
		t.Fatal(err)
	}
	dir := ctx.Args.DataDir
	cliRun(t, ctx, "prices", "--offline", "--no-sync", "--source", "claude")
	db, err := openSQLite(filepath.Join(dir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, err := db.userVersion(); err != nil || v != pricingRulesVersion {
		t.Errorf("user_version = %d (err=%v)，期望 %d", v, err, pricingRulesVersion)
	}
	cost, err := db.Scalar("SELECT cost FROM events")
	if err != nil {
		t.Fatal(err)
	}
	if cost != nil && toFloat(cost) == -1 {
		t.Errorf("cost 仍为 -1，未按新规则重算")
	}
}

func TestNewUnknownModelAfterAlignmentTriggersRefresh(t *testing.T) {
	ctx := newTestContext(t)
	cliMustExec(t, ctx, "UPDATE price_checks SET checked_at = checked_at - 7200")
	cliMustExec(t, ctx, "INSERT INTO events (source, ukey, ts, file, model, tier, input_tokens, cached_input_tokens, "+
		"cache_write_5m_tokens, cache_write_1h_tokens, output_tokens, reasoning_tokens, limit_id) "+
		"VALUES ('claude', 'u', 4102444800, 'f', 'mystery-1', 'standard', 1, 0, 0, 0, 1, 0, 'seven_day')")
	var fetched []string
	orig := fetchText
	fetchText = func(url string) (string, error) {
		fetched = append(fetched, url)
		return "", nil
	}
	defer func() { fetchText = orig }()
	cliRun(t, ctx, "prices", "--no-sync", "--source", "claude")
	want := []string{sources["claude"].PricingURL()}
	if !slices.Equal(fetched, want) {
		t.Errorf("fetched = %q，期望 %q", fetched, want)
	}
}

func TestPricingURLRequiresSingleSource(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"--pricing-url", "file:///x"}, &out, &errBuf); code != 2 {
		t.Errorf("退出码 = %d，期望 2", code)
	}
	if !strings.Contains(errBuf.String(), "--pricing-url 需要配合单个 --source") {
		t.Errorf("stderr 缺少提示: %q", errBuf.String())
	}
}

func TestParseArgsRejectsBadArguments(t *testing.T) {
	for _, c := range []struct {
		argv []string
		msg  string
	}{
		{[]string{"status", "daily"}, "unrecognized arguments: daily"},
		{[]string{"bogus"}, "argument command: invalid choice: 'bogus' (choose from 'status', 'history', 'daily', 'prices', 'segments', 'time', 'sync', 'export', 'import', 'peer', 'record', 'hook')"},
		{[]string{"hook"}, "argument hook: 要给出事件名（session-start）"},
		{[]string{"hook", "stop"}, "argument hook: invalid choice: 'stop' (choose from 'session-start')"},
		{[]string{"hook", "session-start", "x"}, "unrecognized arguments: x"},
		{[]string{"status", "--account", "claude"}, "argument --account: 只用于 record，且要是账号名: 'claude'"},
		{[]string{"record", "--account", "@target"}, "argument --account: 只用于 record，且要是账号名: '@target'"},
		{[]string{"--source", "Nope"}, "argument --source: 不是账号名: 'Nope'"},
		{[]string{"--source", "@target"}, "argument --source: 不是账号名: '@target'"},
		{[]string{"status", "--after", "x"}, "unrecognized arguments: --after"},
		{[]string{"export", "--text"}, "export、import、peer 不接受 --text 与 --source"},
		{[]string{"--days", "x"}, "argument --days: invalid int value: 'x'"},
		{[]string{"--max-price-age", "x"}, "argument --max-price-age: invalid float value: 'x'"},
		{[]string{"status", "--since", "2026-09-01"}, "--since、--until、--split 只用于 segments 与 time"},
		{[]string{"segments", "--since", "2026-09-03", "--until", "2026-09-01"}, "--since 须早于 --until"},
	} {
		var stdout, stderr bytes.Buffer
		args, code := parseArgs(c.argv, &stdout, &stderr)
		if args != nil || code != 2 {
			t.Errorf("%v: args = %+v、退出码 %d，期望 nil 与 2", c.argv, args, code)
		}
		if want := usageLine + "agent-usage: error: " + c.msg + "\n"; stderr.String() != want {
			t.Errorf("%v: stderr = %q，期望 %q", c.argv, stderr.String(), want)
		}
	}
}

func TestParseArgsReadsValues(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args, code := parseArgs([]string{"daily", "--days", "3", "--max-price-age", "2.5", "--source", "claude-b"}, &stdout, &stderr)
	if code != 0 || args == nil {
		t.Fatalf("退出码 %d，stderr: %s", code, stderr.String())
	}
	if args.Command != "daily" || args.Days != 3 || args.MaxPriceAge != 2.5 || !slices.Equal(args.Sources, []string{"claude-b"}) {
		t.Errorf("args = %+v，期望 daily、3 天、2.5、[claude-b]", args)
	}
}

// 按日趋势里一天的 models 按当天金额从高到低列出；没有价格的模型排在最后。
func TestDailyModelsOrderedByCostDescending(t *testing.T) {
	ctx := newTestContext(t)
	ts := float64(time.Now().Unix())
	for i, row := range []struct {
		model string
		cost  any
	}{{"cheap", 0.5}, {"unpriced", nil}, {"dear", 7.25}, {"mid", 2.0}} {
		if _, err := ctx.DB.Exec("INSERT INTO events (source, ukey, ts, file, model, tier, input_tokens, cached_input_tokens, "+
			"cache_write_5m_tokens, cache_write_1h_tokens, output_tokens, reasoning_tokens, cost) "+
			"VALUES ('claude', ?, ?, 'f', ?, 'standard', 1, 0, 0, 0, 1, 0, ?)", "u"+strconv.Itoa(i), ts, row.model, row.cost); err != nil {
			t.Fatal(err)
		}
	}
	days, err := dailyReport(ctx.DB, sources["claude"], 1)
	if err != nil || len(days) != 1 {
		t.Fatalf("days = %v, err = %v", days, err)
	}
	b, err := json.Marshal(days[0].Models)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"dear":7.25,"mid":2,"cheap":0.5,"unpriced":0}`; string(b) != want {
		t.Errorf("models = %s，期望 %s", b, want)
	}
}

// sources 的键按来源名排序（JSON 由 encoding/json 排序），与 --source 的书写顺序无关；--text 按同一顺序分段。
func TestSourcesListedByName(t *testing.T) {
	ctx := newTestContext(t)
	if err := ctx.DB.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := ctx.Args
	if code := run([]string{"prices", "--offline", "--no-sync", "--text", "--source", "claude-b", "--source", "claude",
		"--data-dir", a.DataDir, "--claude-home", a.ClaudeHome}, &stdout, &stderr); code != 0 {
		t.Fatalf("退出码 %d：%s", code, stderr.String())
	}
	text := stdout.String()
	if i, j := strings.Index(text, "━━ claude (USD)"), strings.Index(text, "━━ claude-b (USD)"); i < 0 || j < 0 || i > j {
		t.Errorf("--text 的来源顺序不对:\n%s", text)
	}
}
