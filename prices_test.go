// 价格页解析与计价。样本是 2026-11-14 保存的官方页面，
// 页面改版导致解析器要改时同步更新样本。
package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func priceByKey(rows []PriceRow) map[[2]string]PriceRow {
	out := make(map[[2]string]PriceRow, len(rows))
	for _, r := range rows {
		out[[2]string{r.Model, r.Tier}] = r
	}
	return out
}

// priceRow 取 (model, tier) 一行，缺失时让测试失败。
func priceRow(t *testing.T, rows map[[2]string]PriceRow, model, tier string) PriceRow {
	t.Helper()
	r, ok := rows[[2]string{model, tier}]
	if !ok {
		t.Fatalf("价格表缺少 (%s, %s)", model, tier)
	}
	return r
}

// priceF 解引用价格指针，nil 记为失败并返回 -1。
func priceF(t *testing.T, field string, p *float64) float64 {
	t.Helper()
	if p == nil {
		t.Errorf("%s 为 nil", field)
		return -1
	}
	return *p
}

func priceCost(t *testing.T, book *PriceBook, ev *Event) float64 {
	t.Helper()
	amount, _ := book.Cost(ev)
	if amount == nil {
		t.Fatalf("请求 %+v 没有算出金额", ev)
	}
	return *amount
}

func TestAnthropicCacheColumnsAndFastMode(t *testing.T) {
	rows := priceByKey(sources["claude"].ParsePrices(fixtureText(t, "anthropic-pricing.md")))
	fable := priceRow(t, rows, "Claude Fable 5.1", "standard")
	got := [5]float64{
		fable.Input, priceF(t, "cache_write", fable.CacheWrite), priceF(t, "cache_write_1h", fable.CacheWrite1h),
		priceF(t, "cached_input", fable.CachedInput), fable.Output,
	}
	if got != [5]float64{10, 12.5, 20, 0.25, 50} {
		t.Errorf("Claude Fable 5.1 = %v，期望 [10 12.5 20 0.25 50]", got)
	}
	priceRow(t, rows, "Claude Opus 4.1", "standard") // 名称后的「(retired …)」链接已剥掉
	fast := priceRow(t, rows, "Claude Opus 5", "fast")
	if fast.Input != 10 || fast.Output != 50 {
		t.Errorf("Claude Opus 5 fast = (%v, %v)，期望 (10, 50)", fast.Input, fast.Output)
	}
	if c := priceF(t, "cached_input", fast.CachedInput); !approx(c, 1.0) { // 缓存价沿用标准档对输入价的倍率
		t.Errorf("Claude Opus 5 fast cached_input = %v，期望 1.0", c)
	}
	priceRow(t, rows, "Claude Opus 4.8", "fast") // 表里写作 "Claude Opus 5 / Claude Opus 4.8"
	for k := range rows {
		if k[1] != "standard" && k[1] != "fast" {
			t.Errorf("出现意外档位: %v", k)
		}
	}
}

func TestClaudeModelKeyMatchesDisplayName(t *testing.T) {
	key := sources["claude"].ModelKey
	cases := []struct{ logged, display string }{
		{"claude-fable-5-1", "Claude Fable 5.1"},
		{"claude-haiku-4-5-20251001", "Claude Haiku 4.5"},
		{"claude-opus-4-1-20250805", "Claude Opus 4.1"},
		{"claude-3-5-haiku-20241022", "Claude Haiku 3.5"},
		{"claude-opus-5[1m]", "Claude Opus 5"},
	}
	for _, tc := range cases {
		t.Run(tc.logged, func(t *testing.T) {
			if a, b := key(tc.logged), key(tc.display); a != b {
				t.Errorf("key(%q)=%q ≠ key(%q)=%q", tc.logged, a, tc.display, b)
			}
			if key("claude-opus-5") == key("claude-opus-4-5") {
				t.Error("claude-opus-5 与 claude-opus-4-5 不应同键")
			}
		})
	}
}

func TestResolveMarksBorrowedPrices(t *testing.T) {
	ctx := newTestContext(t)
	book, err := ctx.Book(sources["claude"], false)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		model     string
		want      string // "" 表示没有价格
		wantExact bool
	}{
		{"claude-opus-4-1-20250805", "Claude Opus 4.1", true},
		{"claude-opus-5[1m]", "Claude Opus 5", true},
		// 归一后对不上、但以价格表模型名加连字符开头的名字回落到最长前缀
		{"Claude Opus 5-preview", "Claude Opus 5", false},
		{"some-unknown-model", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got, exact := book.Resolve(strp(tc.model))
			gotName := ""
			if got != nil {
				gotName = *got
			}
			if (got == nil) != (tc.want == "") || gotName != tc.want || exact != tc.wantExact {
				t.Errorf("Resolve(%q) = (%v, %v)，期望 (%q, %v)", tc.model, priceStrOrNil(got), exact, tc.want, tc.wantExact)
			}
		})
	}
}

func priceStrOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestCostSplitsCacheTiers(t *testing.T) {
	ctx := newTestContext(t)
	book, err := ctx.Book(sources["claude"], false)
	if err != nil {
		t.Fatal(err)
	}
	ev := &Event{
		Model: strp("claude-fable-5"), Tier: "standard", Ts: 1.0, InputTokens: 216124, CachedInputTokens: 198530,
		CacheWrite1hTokens: 17592, OutputTokens: 5960,
	}
	// 2 个新输入 ×$10 + 198530 缓存命中 ×$1 + 17592 一小时缓存写入 ×$20 + 5960 输出 ×$50
	if got := priceCost(t, book, ev); !approx(got, 0.84839) {
		t.Fatalf("金额 = %v，期望 0.84839", got)
	}
}

// priceChangedPage 把样本页里第一行 model 的基础输入价从 from 改成 to，写到临时文件并返回路径。
func priceChangedPage(t *testing.T, model, from, to string) string {
	t.Helper()
	lines := strings.Split(fixtureText(t, "anthropic-pricing.md"), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "| "+model+" ") && strings.Contains(line, from) {
			lines[i] = strings.Replace(line, from, to, 1)
			path := filepath.Join(t.TempDir(), "pricing.md")
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
				t.Fatal(err)
			}
			return path
		}
	}
	t.Fatalf("样本页里找不到 %s 的 %q", model, from)
	return ""
}

func TestPriceChangeCreatesNewVersion(t *testing.T) {
	ctx := newTestContext(t)
	src := sources["claude"]
	changed := priceChangedPage(t, "Claude Opus 5", "| $5 / MTok ", "| $6 / MTok ")
	var warnings []string
	if ok, err := refreshPrices(ctx.DB, src, fileURL(changed), &warnings); err != nil || !ok {
		t.Fatalf("refreshPrices = %v, %v %v", ok, err, warnings)
	}
	versions := mustQuery(t, ctx,
		"SELECT valid_from, input FROM prices WHERE source='claude' AND model='Claude Opus 5' "+
			"AND tier='standard' ORDER BY valid_from")
	type ver struct {
		later bool
		input float64
	}
	var got []ver
	for _, v := range versions {
		got = append(got, ver{v.Float("valid_from") > 0, v.Float("input")})
	}
	if len(got) != 2 || got[0] != (ver{false, 5.0}) || got[1] != (ver{true, 6.0}) {
		t.Fatalf("价格版本 = %+v，期望 [{false 5} {true 6}]", got)
	}
	// 再加一个带长上下文价的版本（价格页上 Claude Opus 5 不分档，直接写库）
	if _, err := ctx.DB.Exec("INSERT INTO prices (source, model, tier, valid_from, input, output, long_input, " +
		"long_output, long_threshold, url) VALUES ('claude', 'Claude Opus 5', 'standard', 5e9, 6, 30, 12, 45, 200000, 'test')"); err != nil {
		t.Fatal(err)
	}
	book, err := ctx.Book(src, true)
	if err != nil {
		t.Fatal(err)
	}
	event := func(ts float64, input int64) *Event {
		return &Event{Model: strp("claude-opus-5"), Tier: "standard", Ts: ts, InputTokens: input}
	}
	if got := priceCost(t, book, event(1.0, 100_000)); !approx(got, 0.5) { // 变价前的请求仍按 $5
		t.Errorf("变价前金额 = %v，期望 0.5", got)
	}
	if got := priceCost(t, book, event(4.5e9, 100_000)); !approx(got, 0.6) {
		t.Errorf("变价后金额 = %v，期望 0.6", got)
	}
	if got := priceCost(t, book, event(6e9, 100_000)); !approx(got, 0.6) { // 不超过阈值按基础价
		t.Errorf("阈值内金额 = %v，期望 0.6", got)
	}
	if got := priceCost(t, book, event(6e9, 300_000)); !approx(got, 3.6) { // 超过 200K 走长上下文价 $12
		t.Errorf("长上下文请求金额 = %v，期望 3.6", got)
	}
}

func TestTruncatedPageKeepsOldPrices(t *testing.T) {
	ctx := newTestContext(t)
	broken := filepath.Join(t.TempDir(), "broken.md")
	if err := os.WriteFile(broken, []byte("# Pricing\n\nmoved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	ok, err := refreshPrices(ctx.DB, sources["claude"], fileURL(broken), &warnings)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("截断的价格页不应对齐成功")
	}
	if len(warnings) == 0 {
		t.Fatal("截断的价格页应留下警告")
	}
	book, err := ctx.Book(sources["claude"], true)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := book.Resolve(strp("claude-opus-5")); got == nil || *got != "Claude Opus 5" {
		t.Fatalf("旧价格丢失: Resolve = %v", got)
	}
}

// 按提示长度分档的模型在价格页占两行，解析成一条：「up to」一行是基础价，「over」一行是超过阈值时的长上下文价。
func TestPromptLengthRowsFoldIntoOneModel(t *testing.T) {
	parsed := sources["claude"].ParsePrices(fixtureText(t, "anthropic-pricing.md"))
	rows := priceByKey(parsed)
	haiku := priceRow(t, rows, "Claude Haiku 5.5", "standard")
	base := [5]float64{
		haiku.Input, priceF(t, "cache_write", haiku.CacheWrite), priceF(t, "cache_write_1h", haiku.CacheWrite1h),
		priceF(t, "cached_input", haiku.CachedInput), haiku.Output,
	}
	if base != [5]float64{0.1, 0.125, 0.2, 0.01, 0.5} {
		t.Errorf("Claude Haiku 5.5 基础价 = %v，期望 [0.1 0.125 0.2 0.01 0.5]", base)
	}
	long := [5]float64{
		priceF(t, "long_input", haiku.LongInput), priceF(t, "long_cache_write", haiku.LongCacheWrite),
		priceF(t, "long_cache_write_1h", haiku.LongCacheWrite1h), priceF(t, "long_cached_input", haiku.LongCachedInput),
		priceF(t, "long_output", haiku.LongOutput),
	}
	if long != [5]float64{0.5, 0.625, 1, 0.05, 2.5} {
		t.Errorf("Claude Haiku 5.5 长上下文价 = %v，期望 [0.5 0.625 1 0.05 2.5]", long)
	}
	if haiku.LongThreshold == nil || *haiku.LongThreshold != 100_000 {
		t.Errorf("Claude Haiku 5.5 阈值 = %v，期望 100000", haiku.LongThreshold)
	}
	// 不分档的模型没有长上下文价；每个模型键只对应一个名字，名字里不留分档说明
	if opus := priceRow(t, rows, "Claude Opus 5.5", "standard"); opus.LongInput != nil || opus.LongThreshold != nil {
		t.Errorf("Claude Opus 5.5 不该有长上下文价: %+v", opus)
	}
	names := map[string]string{}
	for _, r := range parsed {
		key := sources["claude"].ModelKey(r.Model)
		if prev, ok := names[key]; ok && prev != r.Model {
			t.Errorf("%q 与 %q 对应同一个模型键 %s", prev, r.Model, key)
		}
		names[key] = r.Model
		if strings.Contains(r.Model, "(") {
			t.Errorf("模型名带括号说明: %q", r.Model)
		}
	}
	// 价格后带脚注上标、或写作「$16 USD / MTok」的单元格照常解析
	if sonnet := priceRow(t, rows, "Claude Sonnet 5", "standard"); sonnet.Input != 2 || sonnet.Output != 10 {
		t.Errorf("Claude Sonnet 5 = (%v, %v)，期望 (2, 10)", sonnet.Input, sonnet.Output)
	}
	if fast := priceRow(t, rows, "Claude Fable 5.2", "fast"); fast.Input != 16 || fast.Output != 80 {
		t.Errorf("Claude Fable 5.2 fast = (%v, %v)，期望 (16, 80)", fast.Input, fast.Output)
	}
}

// 提示长度算全部输入（含读缓存与写缓存）；恰好等于阈值按基础价，超过才按长上下文价，1 小时缓存写入也用长档的价。
func TestPromptLengthPricesWholeRequest(t *testing.T) {
	ctx := newTestContext(t)
	book, err := ctx.Book(accountSource("claude-b"), false)
	if err != nil {
		t.Fatal(err)
	}
	event := func(input int64) *Event {
		return &Event{
			Model: strp("claude-haiku-5-5"), Tier: "standard", Ts: 1.0, InputTokens: input, CachedInputTokens: 60_000,
			CacheWrite5mTokens: 10_000, CacheWrite1hTokens: 30_000, OutputTokens: 1_000,
		}
	}
	// 0 新输入 + 60000 读缓存 ×$0.01 + 10000 五分钟写入 ×$0.125 + 30000 一小时写入 ×$0.20 + 1000 输出 ×$0.50
	if got := priceCost(t, book, event(100_000)); !approx(got, 0.00835) {
		t.Errorf("等于阈值的金额 = %v，期望 0.00835", got)
	}
	// 1 新输入 ×$0.50 + 60000 ×$0.05 + 10000 ×$0.625 + 30000 ×$1 + 1000 ×$2.50
	if got := priceCost(t, book, event(100_001)); !approx(got, 0.0417505) {
		t.Errorf("超过阈值的金额 = %v，期望 0.0417505", got)
	}
}

// 价格页给模型换了写法：库里旧名字的价格行删掉，引用它们的请求（不分来源）清空金额后按新行重算；
// 价格页上已经没有的模型，价格行留着给历史请求用。
func TestRenamedPriceRowsAreReplaced(t *testing.T) {
	ctx := newTestContext(t)
	src := accountSource("claude-b")
	mustAddEvent(t, ctx, src, testEvent("renamed", 1.0, "f", "claude-haiku-5-5", "", 1_000_000))
	for _, model := range []string{"Claude Haiku 5.5 (for prompts over 100,000 tokens)", "Claude Haiku 3.5"} {
		if _, err := ctx.DB.Exec("INSERT INTO prices (source, model, tier, valid_from, input, output, url) "+
			"VALUES ('claude', ?, 'standard', 0, 0.8, 4, 'test')", model); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ctx.DB.Exec("UPDATE events SET cost=99, price_id=(SELECT id FROM prices WHERE model LIKE '%(for prompts%')"); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	if ok, err := refreshPrices(ctx.DB, sources["claude"], fileURL(fixturePath(t, "anthropic-pricing.md")), &warnings); err != nil || !ok {
		t.Fatalf("refreshPrices = %v, %v %v", ok, err, warnings)
	}
	if n := count(t, ctx, "prices", "model LIKE '%(for prompts%'"); n != 0 {
		t.Errorf("旧名字的价格行还剩 %d 条", n)
	}
	if n := count(t, ctx, "prices", "model='Claude Haiku 3.5'"); n != 1 {
		t.Errorf("价格页上没有的模型，价格行 = %d 条，期望保留 1 条", n)
	}
	if n := count(t, ctx, "events", "cost IS NULL AND price_id IS NULL"); n != 1 {
		t.Fatalf("引用旧价格行的请求没有清空金额")
	}
	if err := ctx.RepriceMissing(src); err != nil {
		t.Fatal(err)
	}
	// 100 万输入超过阈值，按长上下文价 $0.50
	got := mustQuery(t, ctx, "SELECT e.cost, p.model FROM events e JOIN prices p ON p.id=e.price_id")
	if len(got) != 1 || !approx(got[0].Float("cost"), 0.5) || got[0].Str("model") != "Claude Haiku 5.5" {
		t.Errorf("重算结果 = %v，期望金额 0.5、价格行 Claude Haiku 5.5", got)
	}
}

// 两个名字对应同一个模型键的页面按结构变化处理：不对齐、留警告，库里的价格原样不动。
func TestCollidingModelNamesKeepOldPrices(t *testing.T) {
	ctx := newTestContext(t)
	const wording = "(for prompts over 100,000 tokens)"
	page := fixtureText(t, "anthropic-pricing.md")
	if !strings.Contains(page, wording) {
		t.Fatalf("样本页里找不到 %q", wording)
	}
	path := filepath.Join(t.TempDir(), "pricing.md")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(page, wording, "(long prompts)")), 0o644); err != nil {
		t.Fatal(err)
	}
	before := mustQuery(t, ctx, "SELECT * FROM prices ORDER BY id")
	var warnings []string
	ok, err := refreshPrices(ctx.DB, sources["claude"], fileURL(path), &warnings)
	if err != nil {
		t.Fatal(err)
	}
	if ok || len(warnings) != 1 || !strings.Contains(warnings[0], "对应同一个模型") {
		t.Fatalf("refreshPrices = %v，警告 %v；期望对齐失败并说明两个名字对应同一个模型", ok, warnings)
	}
	if after := mustQuery(t, ctx, "SELECT * FROM prices ORDER BY id"); !reflect.DeepEqual(after, before) {
		t.Error("对齐失败时改动了价格表")
	}
}
