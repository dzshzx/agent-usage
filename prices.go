// 官方价格页的抓取、版本落库、按需对齐与按请求计价。
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// fetchText 取回 URL 的正文；file:// 直接读文件。测试可替换。
var fetchText = func(url string) (string, error) {
	if strings.HasPrefix(url, "file://") {
		b, err := os.ReadFile(url[7:])
		return string(b), err
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (agent-usage)")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.ToValidUTF8(string(b), "�"), nil
}

// refreshPrices 抓取并解析价格页，有变化的 (model, tier) 新增一个版本，价格页换了写法的旧名字清掉；返回是否成功。
func refreshPrices(db *DB, src Source, url string, warnings *[]string) (bool, error) {
	ts := now()
	rows, err := parsePricePage(src, url)
	if err != nil {
		if _, e := db.Exec("INSERT INTO price_checks VALUES (?, ?, 0, NULL, NULL, ?)", src.Name(), ts, truncateRunes(err.Error(), 300)); e != nil {
			return false, e
		}
		*warnings = append(*warnings, fmt.Sprintf("%s: 价格对齐失败，沿用已采集价格: %s", src.Name(), err))
		return false, nil
	}
	if err := db.Tx(func() error { return storePrices(db, src, url, ts, rows) }); err != nil {
		return false, err
	}
	return true, nil
}

// storePrices 在 refreshPrices 的事务里写入解析出的价格：清掉换了写法的旧名字，有变化的 (model, tier) 新增一版，记一条成功的对齐。
func storePrices(db *DB, src Source, url string, ts float64, rows []PriceRow) error {
	first, err := db.QueryRow("SELECT 1 FROM prices WHERE source=? LIMIT 1", src.Name())
	if err != nil {
		return err
	}
	firstLoad := first == nil
	if err := dropRenamedPrices(db, src, rows); err != nil {
		return err
	}
	changed := 0
	cols := strings.Join(priceFields, ", ")
	for _, row := range rows {
		cur, err := db.QueryRow(
			"SELECT "+cols+" FROM prices WHERE source=? AND model=? AND tier=? ORDER BY valid_from DESC LIMIT 1",
			src.Name(), row.Model, row.Tier)
		if err != nil {
			return err
		}
		vals := row.values()
		if cur != nil && samePrices(cur, vals) {
			continue
		}
		// 首次采集的价格覆盖全部历史；之后的变价从发现时刻起生效。新出现的模型同样回溯。
		validFrom := 0.0
		if !firstLoad && cur != nil {
			validFrom = ts
		}
		args := append([]any{src.Name(), row.Model, row.Tier, validFrom}, vals...)
		args = append(args, url)
		if _, err := db.Exec(
			"INSERT INTO prices (source, model, tier, valid_from, "+cols+", url) VALUES (?, ?, ?, ?, "+
				placeholders(len(priceFields))+", ?)", args...); err != nil {
			return err
		}
		changed++
	}
	_, err = db.Exec("INSERT INTO price_checks VALUES (?, ?, 1, ?, ?, NULL)", src.Name(), ts, len(rows), changed)
	return err
}

func parsePricePage(src Source, url string) ([]PriceRow, error) {
	text, err := fetchText(url)
	if err != nil {
		return nil, err
	}
	rows := src.ParsePrices(text)
	// 页面改版导致解析结果过少时保留旧价格，不写入残缺数据
	if len(rows) < 3 {
		return nil, fmt.Errorf("只解析出 %d 条价格，疑似页面结构变化", len(rows))
	}
	// 两个名字对应同一个模型键时，请求该按哪条计价无从判断，同样保留旧价格
	names := map[string]string{}
	for _, r := range rows {
		key := src.ModelKey(r.Model)
		if prev, ok := names[key]; ok && prev != r.Model {
			return nil, fmt.Errorf("「%s」与「%s」对应同一个模型，疑似页面结构变化", prev, r.Model)
		}
		names[key] = r.Model
	}
	return rows, nil
}

// dropRenamedPrices 删掉库里与价格页某个模型同键、名字却不同的价格行（价格页给这个模型换了写法），
// 旧名字的历史版本一并删掉；引用它们的请求清空金额，进报表的来源随后由 RepriceMissing 按新行重算，
// 只入库不进报表的非账号目标留空。库里每个模型键因此只对应一个名字。
func dropRenamedPrices(db *DB, src Source, rows []PriceRow) error {
	current := map[string]string{}
	for _, r := range rows {
		current[src.ModelKey(r.Model)] = r.Model
	}
	stored, err := db.Query("SELECT DISTINCT model FROM prices WHERE source=?", src.Name())
	if err != nil {
		return err
	}
	for _, s := range stored {
		old := s.Str("model")
		if name, ok := current[src.ModelKey(old)]; !ok || name == old {
			continue
		}
		if _, err := db.Exec("UPDATE events SET cost=NULL, price_id=NULL WHERE price_id IN "+
			"(SELECT id FROM prices WHERE source=? AND model=?)", src.Name(), old); err != nil {
			return err
		}
		if _, err := db.Exec("DELETE FROM prices WHERE source=? AND model=?", src.Name(), old); err != nil {
			return err
		}
	}
	return nil
}

// samePrices 比较库里最新一版与新解析的价格列是否完全相同（NULL 只与 NULL 相同）。
func samePrices(cur Row, vals []any) bool {
	for i, f := range priceFields {
		a, b := cur[f], vals[i]
		if (a == nil) != (b == nil) {
			return false
		}
		if a != nil && toFloat(a) != toFloat(b) {
			return false
		}
	}
	return true
}

// PriceBook 是一个价格表来源的价格版本；传入共用价格表的来源时按其 price owner 读取。
type PriceBook struct {
	src      Source
	versions map[[2]string][]priceVersion
	byKey    map[string]string
	byLength []string
	resolved map[string]resolvedModel // Resolve 的结果按模型名记下：同步导入几十万行时模型名只有几种
}

type resolvedModel struct {
	name  *string
	exact bool
}

func newPriceBook(db *DB, src Source) (*PriceBook, error) {
	src = priceOwner(src)
	rows, err := db.Query("SELECT * FROM prices WHERE source=? ORDER BY valid_from", src.Name())
	if err != nil {
		return nil, err
	}
	book := &PriceBook{src: src, versions: map[[2]string][]priceVersion{}, byKey: map[string]string{}, resolved: map[string]resolvedModel{}}
	seen := map[string]bool{}
	for _, r := range rows {
		k := [2]string{r.Str("model"), r.Str("tier")}
		book.versions[k] = append(book.versions[k], newPriceVersion(r))
		seen[r.Str("model")] = true
	}
	for m := range seen {
		book.byKey[src.ModelKey(m)] = m
		book.byLength = append(book.byLength, m)
	}
	// 同长度的模型名顺序无关紧要：前缀回落只看最长匹配
	sort.SliceStable(book.byLength, func(i, j int) bool { return len(book.byLength[i]) > len(book.byLength[j]) })
	return book, nil
}

// Resolve 返回 (价格表里的模型名, 是否精确对应)。前缀回落得到的价格不算精确。
func (b *PriceBook) Resolve(model *string) (*string, bool) {
	if model == nil || *model == "" {
		return nil, false
	}
	r, ok := b.resolved[*model]
	if !ok {
		r = b.resolve(*model)
		b.resolved[*model] = r
	}
	return r.name, r.exact
}

func (b *PriceBook) resolve(model string) resolvedModel {
	if hit, ok := b.byKey[b.src.ModelKey(model)]; ok {
		return resolvedModel{strp(hit), true}
	}
	// 带日期或变体后缀的模型名回落到价格表里最长的前缀
	for _, m := range b.byLength {
		if strings.HasPrefix(model, m+"-") {
			return resolvedModel{strp(m), false}
		}
	}
	return resolvedModel{}
}

// unitRates 是一组单价（每 1M tokens）。
type unitRates struct{ in, cached, write5m, write1h, out float64 }

// priceVersion 是价格表的一行（一个模型、档位的一个版本），单价按普通与长上下文两档先算好：
// 同步导入几十万行时每行只做算术。
type priceVersion struct {
	id            int64
	validFrom     float64
	longThreshold int64 // 0 表示没有长上下文价
	normal, long  unitRates
}

func newPriceVersion(p Row) priceVersion {
	unit := func(k string) unitRates {
		r := unitRates{in: p.Float(k + "input"), out: p.Float("output")}
		if !p.IsNull(k + "output") {
			r.out = p.Float(k + "output")
		}
		r.cached, r.write5m = r.in, r.in
		if !p.IsNull(k + "cached_input") {
			r.cached = p.Float(k + "cached_input")
		}
		if !p.IsNull(k + "cache_write") {
			r.write5m = p.Float(k + "cache_write")
		}
		r.write1h = r.write5m
		if !p.IsNull(k + "cache_write_1h") {
			r.write1h = p.Float(k + "cache_write_1h")
		}
		return r
	}
	v := priceVersion{id: p.Int("id"), validFrom: p.Float("valid_from"), normal: unit("")}
	if !p.IsNull("long_input") && p.Int("long_threshold") != 0 {
		v.longThreshold, v.long = p.Int("long_threshold"), unit("long_")
	}
	return v
}

// priceRates 是一次请求适用的各类单价与按单价切开的 token 数。
type priceRates struct {
	unitRates
	fresh, cachedN, write5mN, write1hN, outN int64
}

// Cost 按请求当时生效的价格版本计算金额；没有价格时两者都为 nil。
func (b *PriceBook) Cost(ev *Event) (*float64, *int64) {
	r, id, ok := b.rates(ev)
	if !ok {
		return nil, nil
	}
	amount := (float64(r.fresh)*r.in + float64(r.cachedN)*r.cached + float64(r.write5mN)*r.write5m +
		float64(r.write1hN)*r.write1h + float64(r.outN)*r.out) / 1_000_000
	return floatp(amount), intp(id)
}

// Parts 把一次请求的金额拆成新输入、读缓存、写缓存与输出四部分；没有价格时为 nil。
func (b *PriceBook) Parts(ev *Event) *CostParts {
	r, _, ok := b.rates(ev)
	if !ok {
		return nil
	}
	return &CostParts{
		FreshInput: float64(r.fresh) * r.in / 1_000_000,
		CacheRead:  float64(r.cachedN) * r.cached / 1_000_000,
		CacheWrite: (float64(r.write5mN)*r.write5m + float64(r.write1hN)*r.write1h) / 1_000_000,
		Output:     float64(r.outN) * r.out / 1_000_000,
	}
}

// rates 取请求当时生效的价格版本，算出各类单价与对应 token 数，并返回该版本的编号；没有价格时 ok 为假。
func (b *PriceBook) rates(ev *Event) (r priceRates, id int64, ok bool) {
	name, _ := b.Resolve(ev.Model)
	if name == nil {
		return r, 0, false
	}
	versions := b.versions[[2]string{*name, ev.Tier}]
	if len(versions) == 0 {
		versions = b.versions[[2]string{*name, "standard"}]
	}
	if len(versions) == 0 {
		return r, 0, false
	}
	p := &versions[0]
	for i := range versions {
		if versions[i].validFrom <= ev.Ts {
			p = &versions[i]
		}
	}
	r.unitRates = p.normal
	if b.src.LongContextPremium() && p.longThreshold != 0 && ev.InputTokens > p.longThreshold {
		r.unitRates = p.long
	}
	r.cachedN = min(ev.CachedInputTokens, ev.InputTokens)
	r.write5mN = min(ev.CacheWrite5mTokens, ev.InputTokens-r.cachedN)
	r.write1hN = min(ev.CacheWrite1hTokens, ev.InputTokens-r.cachedN-r.write5mN)
	r.fresh = ev.InputTokens - r.cachedN - r.write5mN - r.write1hN
	r.outN = ev.OutputTokens
	return r, p.id, true
}

// lastAlignedAt 返回价格表来源上次对齐成功的时间（epoch 秒）；从未成功时为 nil。
func lastAlignedAt(db *DB, src Source) (*float64, error) {
	v, err := db.Scalar("SELECT MAX(checked_at) FROM price_checks WHERE source=? AND ok=1", priceOwner(src).Name())
	return scalarFloatP(v), err
}

// PriceEntry 是 prices 子命令输出的一条价格。
type PriceEntry struct {
	Model            string   `json:"model"`
	Tier             string   `json:"tier"`
	Input            float64  `json:"input"`
	CachedInput      *float64 `json:"cached_input"`
	CacheWrite       *float64 `json:"cache_write"`
	CacheWrite1h     *float64 `json:"cache_write_1h"`
	Output           float64  `json:"output"`
	LongInput        *float64 `json:"long_input"`
	LongCachedInput  *float64 `json:"long_cached_input"`
	LongCacheWrite   *float64 `json:"long_cache_write"`
	LongCacheWrite1h *float64 `json:"long_cache_write_1h"`
	LongOutput       *float64 `json:"long_output"`
	LongThreshold    *int64   `json:"long_threshold"`
	ValidFrom        *string  `json:"valid_from"`
}

// latestPrices 返回价格表来源每个 (model, tier) 的最新一版价格。
func latestPrices(db *DB, src Source) ([]PriceEntry, error) {
	rows, err := db.Query(
		"SELECT * FROM prices p WHERE source=? AND valid_from = (SELECT MAX(valid_from) FROM prices "+
			"WHERE source=p.source AND model=p.model AND tier=p.tier) ORDER BY model, tier", priceOwner(src).Name())
	if err != nil {
		return nil, err
	}
	out := make([]PriceEntry, 0, len(rows))
	for _, r := range rows {
		var validFrom *string
		if r.Float("valid_from") != 0 {
			validFrom = iso(r.FloatP("valid_from"))
		}
		out = append(out, PriceEntry{
			Model: r.Str("model"), Tier: r.Str("tier"), Input: r.Float("input"),
			CachedInput: r.FloatP("cached_input"), CacheWrite: r.FloatP("cache_write"), CacheWrite1h: r.FloatP("cache_write_1h"),
			Output: r.Float("output"), LongInput: r.FloatP("long_input"), LongCachedInput: r.FloatP("long_cached_input"),
			LongCacheWrite: r.FloatP("long_cache_write"), LongCacheWrite1h: r.FloatP("long_cache_write_1h"),
			LongOutput: r.FloatP("long_output"), LongThreshold: r.IntP("long_threshold"),
			ValidFrom: validFrom,
		})
	}
	return out, nil
}

// PriceState 是一个来源的价格对齐状态。
type PriceState struct {
	URL           string   `json:"url"`
	LastAlignedAt *string  `json:"last_aligned_at"`
	AgeDays       *float64 `json:"age_days"`
	Models        int64    `json:"models"`
	LastAttemptAt *string  `json:"last_attempt_at"`
	LastAttemptOK *bool    `json:"last_attempt_ok"`
	LastError     *string  `json:"last_error"`
	RefreshedNow  bool     `json:"refreshed_now"`
}

func priceState(db *DB, src Source) (*PriceState, error) {
	src = priceOwner(src)
	ok, err := db.QueryRow(
		"SELECT checked_at, models FROM price_checks WHERE source=? AND ok=1 ORDER BY checked_at DESC LIMIT 1", src.Name())
	if err != nil {
		return nil, err
	}
	last, err := db.QueryRow(
		"SELECT checked_at, ok, detail FROM price_checks WHERE source=? ORDER BY checked_at DESC LIMIT 1", src.Name())
	if err != nil {
		return nil, err
	}
	st := &PriceState{URL: src.PricingURL()}
	if ok != nil {
		st.LastAlignedAt = iso(ok.FloatP("checked_at"))
		st.AgeDays = floatp(roundTo((now()-ok.Float("checked_at"))/86400, 2))
		st.Models = ok.Int("models")
	}
	if last != nil {
		st.LastAttemptAt = iso(last.FloatP("checked_at"))
		attemptOK := last.Int("ok") != 0
		st.LastAttemptOK = &attemptOK
		if !attemptOK {
			st.LastError = last.StrP("detail")
		}
	}
	return st, nil
}

// ensurePrices 按需对齐官网价格；返回本次运行是否刷新成功。共用价格表的来源按被共用的来源判断，不另外抓取；
// 一次运行里每本价格表最多抓一次，之后再问都得到那次的结果。上次尝试（成败都算）一小时内不再抓，
// 从没对齐成功过的库也一样：连不上价格页时，不让每次运行（包括作为同步对端被调起）都等一次超时。
func (ctx *Context) ensurePrices(src Source, newModel bool) (bool, error) {
	src = priceOwner(src)
	db, args := ctx.DB, ctx.Args
	if args.Offline {
		return false, nil
	}
	if refreshed, tried := ctx.priceAttempts[src.Name()]; tried {
		return refreshed, nil
	}
	ts := now()
	ok, err := lastAlignedAt(db, src)
	if err != nil {
		return false, err
	}
	attempt, err := db.Scalar("SELECT MAX(checked_at) FROM price_checks WHERE source=?", src.Name())
	if err != nil {
		return false, err
	}
	due := ok == nil || ts-*ok > args.MaxPriceAge*86400
	if !(args.RefreshPrices || due || newModel) {
		return false, nil
	}
	if !args.RefreshPrices && attempt != nil && ts-toFloat(attempt) < priceRetrySeconds {
		return false, nil
	}
	url := src.PricingURL()
	if args.PricingURL != "" {
		url = args.PricingURL
	}
	refreshed, err := refreshPrices(db, src, url, &ctx.Warnings)
	ctx.priceAttempts[src.Name()] = refreshed
	return refreshed, err
}
