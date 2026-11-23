// Claude Code 来源：各账号共用会话日志，按会话账号登记分到各账号的来源。
package main

import (
	"bytes"
	"fmt"
	"github.com/dzshzx/agent-usage/reading"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// primaryAccount 是主号：会话没有登记时归它，会话日志由它扫描、价格表由它抓取，其余账号共用。
const primaryAccount = reading.PrimaryAccount

// targetSource 收 claude-switch 非账号目标（不登录 claude.ai、改用一份设置文件，比如经 API 网关用 API Key）的会话，
// 即登记里带 "target": true 的那几段。它们照常入库，但不登记在 sources 里，报表与额度估算都看不到，也不同步给对端；
// 留在库里是为了 resume 复制件能查到原件、不被归到主号。名字带 @，不合账号名写法，不会和真的账号撞上。
const targetSource = "@target"

// accountNamePattern 是会话账号登记里认作账号的名字，与 claude-switch 的名字规则同一写法。
var accountNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// isAccountName：能当报表来源的账号名（targetSource 不是）。
func isAccountName(name string) bool {
	return accountNamePattern.MatchString(name)
}

var (
	primary   = newClaudeSource()
	targetSrc = primary.cloneAs(targetSource)
)

// accountSource 返回账号 name 的来源，第一次见到时新建并登记进 sources；targetSource 返回不登记的 targetSrc。
func accountSource(name string) Source {
	if name == targetSource {
		return targetSrc
	}
	if s, ok := sources[name]; ok {
		return s
	}
	s := primary.cloneAs(name)
	sources[name] = s
	return s
}

// discoverAccounts 把已知的账号都登记进 sources：读数目录里每个 rate-limits.<账号>.jsonl，
// 以及库里有 7 天窗口请求或读数的来源（旧库里 Codex 等别的限额名的行不算）。会话日志里新出现的账号在入库时登记。
func discoverAccounts(ctx *Context) error {
	matches, err := filepath.Glob(filepath.Join(ctx.Args.DataDir, "rate-limits.*.jsonl"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "rate-limits."), ".jsonl")
		if isAccountName(name) {
			accountSource(name)
		}
	}
	// events 按 source 跳着取不同的值（每个值一次索引查找），不逐行扫几十万行
	rows, err := ctx.DB.Query(`WITH RECURSIVE s(name) AS (
    SELECT MIN(source) FROM events
    UNION ALL SELECT (SELECT MIN(source) FROM events WHERE source > s.name) FROM s WHERE s.name IS NOT NULL)
SELECT name FROM s WHERE name IS NOT NULL AND EXISTS (SELECT 1 FROM events WHERE source=s.name AND limit_id=?)
UNION SELECT DISTINCT source FROM readings WHERE limit_id=?`, weekLimitID, weekLimitID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if name := r.Str("name"); isAccountName(name) {
			accountSource(name)
		}
	}
	return nil
}

type claudeSource struct {
	baseSource
	// 会话日志实际扫描的次数（只在主号上累计：别的账号复用主号的扫描结果，不另扫）
	scans int
}

func newClaudeSource() *claudeSource {
	return &claudeSource{
		baseSource: baseSource{
			name:               primaryAccount,
			currency:           "USD",
			symbol:             "$",
			pricingURL:         "https://platform.claude.com/docs/en/about-claude/pricing.md",
			longContextPremium: true,
		},
	}
}

var (
	modelSuffixRe  = regexp.MustCompile(`[\[(].*$`)
	modelDateRe    = regexp.MustCompile(`-\d{8}$`)
	lettersRe      = regexp.MustCompile(`[a-z]+`)
	digitsRe       = regexp.MustCompile(`\d+`)
	modelBracketRe = regexp.MustCompile(`\[.*$`)
	retiredLinkRe  = regexp.MustCompile(`\s*\(\[.*$`)
	// 按提示长度分档的模型在价格表里占两行，名字后带「(for prompts up to 100,000 tokens)」或「(… over …)」
	promptLengthRe  = regexp.MustCompile(`(?i)^(.+?)\s*\(for prompts (up to|over) ([\d,]+) tokens\)$`)
	claudeMarkers   = [][]byte{[]byte(`"usage"`), []byte(`"assistant"`)}
	fastModeHeaders = []string{"model", "input", "output"}
)

// ModelKey：价格页用展示名（Claude Opus 4.1），日志用 API id（claude-opus-4-1-20250805）；
// 两边都归一成「家族名 + 版本数字」再比对，例如 opus41。
func (s *claudeSource) ModelKey(model string) string {
	model = modelSuffixRe.ReplaceAllString(strings.ToLower(model), "")
	model = modelDateRe.ReplaceAllString(strings.TrimSpace(model), "")
	var letters strings.Builder
	for _, w := range lettersRe.FindAllString(model, -1) {
		if w != "claude" {
			letters.WriteString(w)
		}
	}
	return letters.String() + strings.Join(digitsRe.FindAllString(model, -1), "")
}

type fastPrice struct {
	name   string
	input  float64
	output float64
}

func (s *claudeSource) ParsePrices(text string) []PriceRow {
	var order [][2]string
	rows := map[[2]string]PriceRow{}
	var fast []fastPrice
	var long []PriceRow
	for _, table := range markdownTables(text) {
		fastTable := equalStrings(table.header, fastModeHeaders) && anyContains(table.context, "fast mode")
		for _, cells := range table.rows {
			col := zipColumns(table.header, cells)
			var names []string
			for _, n := range strings.Split(col["model"], " / ") {
				names = append(names, strings.TrimSpace(retiredLinkRe.ReplaceAllString(n, "")))
			}
			if _, ok := col["base input tokens"]; ok {
				long = append(long, standardRows(rows, &order, col, names)...)
			} else if fastTable {
				fast = append(fast, fastPrices(col, names)...)
			}
		}
	}
	longRows(rows, long)
	fastRows(rows, &order, fast)
	out := make([]PriceRow, 0, len(order))
	for _, k := range order {
		out = append(out, rows[k])
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func anyContains(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), sub) {
			return true
		}
	}
	return false
}

// standardRows：标准档价格表的一行：同一行列出的几个模型名共用这组价格，已有的模型不覆盖。
// 按提示长度分档的模型，「up to N」一行是它的基础价；「over N」一行不进 rows，带着阈值返回，由 longRows 并进基础行。
func standardRows(rows map[[2]string]PriceRow, order *[][2]string, col map[string]string, names []string) []PriceRow {
	priceIn, priceOut := parseMoney(col["base input tokens"]), parseMoney(col["output tokens"])
	if priceIn == nil || priceOut == nil {
		return nil
	}
	var long []PriceRow
	for _, n := range names {
		model, over := promptLength(n)
		row := PriceRow{
			Model: model, Tier: "standard", Input: *priceIn,
			CachedInput:   parseMoney(col["cache hits and refreshes"]),
			CacheWrite:    parseMoney(col["5m cache writes"]),
			CacheWrite1h:  parseMoney(col["1h cache writes"]),
			Output:        *priceOut,
			LongThreshold: over,
		}
		if over != nil {
			long = append(long, row)
			continue
		}
		k := [2]string{model, "standard"}
		if _, ok := rows[k]; ok {
			continue
		}
		rows[k] = row
		*order = append(*order, k)
	}
	return long
}

// promptLength 拆开价格表里的模型名：返回去掉分档说明的模型名；是「over N」一行时另返回 N。
func promptLength(name string) (string, *int64) {
	m := promptLengthRe.FindStringSubmatch(name)
	if m == nil {
		return name, nil
	}
	if !strings.EqualFold(m[2], "over") {
		return m[1], nil
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(m[3], ",", ""), 10, 64)
	if err != nil {
		return name, nil
	}
	return m[1], &n
}

// longRows：把「over N」一行的价格并进同名基础行，作为提示超过 N tokens 时的长上下文价；没有基础行的丢弃。
func longRows(rows map[[2]string]PriceRow, long []PriceRow) {
	for _, l := range long {
		k := [2]string{l.Model, "standard"}
		base, ok := rows[k]
		if !ok || base.LongThreshold != nil {
			continue
		}
		base.LongInput, base.LongOutput = floatp(l.Input), floatp(l.Output)
		base.LongCachedInput, base.LongCacheWrite, base.LongCacheWrite1h = l.CachedInput, l.CacheWrite, l.CacheWrite1h
		base.LongThreshold = l.LongThreshold
		rows[k] = base
	}
}

// fastPrices：fast mode 表的一行：(模型名, 输入价, 输出价)；缺价时为空。
func fastPrices(col map[string]string, names []string) []fastPrice {
	priceIn, priceOut := parseMoney(col["input"]), parseMoney(col["output"])
	if priceIn == nil || priceOut == nil {
		return nil
	}
	out := make([]fastPrice, 0, len(names))
	for _, n := range names {
		out = append(out, fastPrice{n, *priceIn, *priceOut})
	}
	return out
}

// fastRows：fast mode 表只给输入输出价；缓存价按官方说明沿用标准档的倍率。
func fastRows(rows map[[2]string]PriceRow, order *[][2]string, fast []fastPrice) {
	for _, f := range fast {
		std, ok := rows[[2]string{f.name, "standard"}]
		if !ok {
			continue
		}
		scale := f.input / std.Input
		row := std
		row.Tier = "fast"
		row.Input = f.input
		row.Output = f.output
		for _, p := range []**float64{&row.CachedInput, &row.CacheWrite, &row.CacheWrite1h} {
			if *p != nil {
				*p = floatp(**p * scale)
			}
		}
		k := [2]string{f.name, "fast"}
		if _, ok := rows[k]; !ok {
			*order = append(*order, k)
		}
		rows[k] = row
	}
}

// Sync：主号扫描一遍会话日志，按会话账号登记分到各账号；别的账号复用本轮的扫描结果。
func (s *claudeSource) Sync(ctx *Context) (SyncStats, error) {
	if s != primary {
		return sessionStats(ctx, s)
	}
	s.scans++
	root := filepath.Join(ctx.Args.ClaudeHome, "projects")
	if !isDir(root) {
		ctx.warn(fmt.Sprintf("claude: 找不到目录 %s", root))
		ctx.claudeSync = &claudeSyncStats{added: map[string]int{}}
		return SyncStats{}, nil
	}
	paths := globRecursive(root, "*.jsonl")
	mtimes := make(map[string]int64, len(paths))
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil {
			mtimes[p] = info.ModTime().UnixNano()
		}
	}
	sort.SliceStable(paths, func(i, j int) bool { return mtimes[paths[i]] < mtimes[paths[j]] })
	accounts, err := s.loadSessionAccounts(ctx)
	if err != nil {
		return SyncStats{}, err
	}
	ctx.sessionAccounts = accounts
	ctx.claudeAdded = map[string]int{}
	ctx.claudeDeferred = nil
	nameOf := func(p string) string {
		rel, _ := filepath.Rel(root, p)
		return rel
	}
	stats, err := ctx.SyncFiles(s, paths, nameOf, s.ingest, s.settleDeferred)
	if err != nil {
		return stats, err
	}
	ctx.claudeSync = &claudeSyncStats{SyncStats: stats, added: ctx.claudeAdded}
	stats.EventsAdded = ctx.claudeAdded[s.name]
	return stats, nil
}

// sessionStats：本轮会话日志扫描里记到 src 的统计；本轮还没扫过时先由主号扫一遍。
func sessionStats(ctx *Context, src Source) (SyncStats, error) {
	if ctx.claudeSync == nil {
		if _, err := primary.Sync(ctx); err != nil {
			return SyncStats{}, err
		}
	}
	stats := ctx.claudeSync.SyncStats
	stats.EventsAdded = ctx.claudeSync.added[src.Name()]
	return stats, nil
}

// loadSessionAccounts：hook session-start 在每次会话启动（含 resume）时追加 {"ts", "session_id", "account"[, "target"]}；按会话分组、按时间排序。
func (s *claudeSource) loadSessionAccounts(ctx *Context) (map[string][]accountRow, error) {
	out := map[string][]accountRow{}
	path := ctx.Args.sessionAccountLog()
	data, err := os.ReadFile(path)
	if err != nil {
		return out, nil
	}
	bad := 0
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := s.loadAccountLine(line, out); err != nil {
			if !isLineError(err) {
				return nil, err
			}
			bad++
		}
	}
	if bad > 0 {
		ctx.warn(fmt.Sprintf("claude: 会话账号登记 %s 有 %d 行无法解析，已跳过", path, bad))
	}
	for _, rows := range out {
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].ts != rows[j].ts {
				return rows[i].ts < rows[j].ts
			}
			return rows[i].account < rows[j].account
		})
	}
	return out, nil
}

func (s *claudeSource) loadAccountLine(line []byte, out map[string][]accountRow) error {
	rec, err := decodeObject(line)
	if err != nil {
		return err
	}
	account, ok := rec["account"]
	if !ok {
		return lineErr("缺少字段 account")
	}
	name, isStr := account.(string)
	if !isStr || !accountNamePattern.MatchString(name) {
		return nil
	}
	if rec["target"] == true {
		name = targetSource
	}
	sessionID, ok := rec["session_id"]
	if !ok {
		return lineErr("缺少字段 session_id")
	}
	ts, err := requireNum(rec, "ts")
	if err != nil {
		return err
	}
	// 会话号与会话日志里的 sessionId 用同一个 jsonText 写成文本再比对
	key := jsonText(sessionID)
	out[key] = append(out[key], accountRow{ts: ts, account: name})
	return nil
}

// accountOf：该会话在 ts 及之前的最后一条登记；会话没有登记（含登记开始前的会话）归主号。
// 会话有登记但都晚于 ts 时返回 nil：这条只可能是 resume 从别的会话复制来的旧消息。
func accountOf(ctx *Context, sessionID string, ts float64) *string {
	rows := ctx.sessionAccounts[sessionID]
	i := sort.Search(len(rows), func(i int) bool { return rows[i].ts > ts })
	if i > 0 {
		return strp(rows[i-1].account)
	}
	if len(rows) > 0 {
		return nil
	}
	return strp(primaryAccount)
}

// settleDeferred：复制件等到本轮全部文件扫完再定归属：原件一定比复制件先写，同一轮里看得到复制件就看得到原件。
// 与读取进度在同一事务里提交，中途出错时整轮回滚、下次重读，复制件不会漏计。
func (s *claudeSource) settleDeferred(ctx *Context) error {
	for _, ev := range ctx.claudeDeferred {
		account, err := recordedAccount(ctx, ev.Ukey)
		if err != nil {
			return err
		}
		name := primaryAccount
		if account != nil {
			name = *account
		}
		added, err := ctx.AddEvent(accountSource(name), ev)
		if err != nil {
			return err
		}
		if added {
			ctx.claudeAdded[name]++
		}
	}
	ctx.claudeDeferred = nil
	return nil
}

// recordedAccount：同一消息已记在哪个已登记的账号（含 targetSource）下；各账号之间也只计一次（库里由唯一索引 events_ukey 兜底）。
func recordedAccount(ctx *Context, ukey string) (*string, error) {
	v, err := ctx.DB.Scalar("SELECT source FROM events WHERE ukey=?", ukey)
	if err != nil || v == nil {
		return nil, err
	}
	if name := jsonText(v); sources[name] != nil || name == targetSource {
		return &name, nil
	}
	return nil, nil
}

func (s *claudeSource) ingest(ctx *Context, path, name string, offset int64, state *fileState) (int64, int, error) {
	return readJSONL(ctx, s.name, path, offset, func(line []byte) (int, error) {
		return s.ingestLine(ctx, path, name, line, state)
	})
}

func (s *claudeSource) ingestLine(ctx *Context, path, name string, line []byte, state *fileState) (int, error) {
	if !bytes.Contains(line, claudeMarkers[0]) || !bytes.Contains(line, claudeMarkers[1]) {
		return 0, nil
	}
	rec, err := decodeObject(line)
	if err != nil {
		return 0, err
	}
	msg, err := objOrEmpty(rec, "message")
	if err != nil {
		return 0, err
	}
	model, isStr := msg["model"].(string)
	if truthy(msg["model"]) && !isStr {
		return 0, wrongType("model", "字符串", msg["model"])
	}
	// <synthetic> 是客户端自己生成的占位消息，没有真实请求
	if strOr(rec, "type") != "assistant" || !truthy(msg["usage"]) || !truthy(msg["id"]) || model == "" || strings.HasPrefix(model, "<") {
		return 0, nil
	}
	usage, err := objOrEmpty(msg, "usage")
	if err != nil {
		return 0, err
	}
	msgID, ok := msg["id"].(string)
	if !ok {
		return 0, wrongType("id", "字符串", msg["id"])
	}
	read, write1h, write5m, err := claudeCacheTokens(usage)
	if err != nil {
		return 0, err
	}
	details, err := objOrEmpty(usage, "output_tokens_details")
	if err != nil {
		return 0, err
	}
	raw, err := requireStr(rec, "timestamp")
	if err != nil {
		return 0, err
	}
	ts, err := parseTs(raw)
	if err != nil {
		return 0, err
	}
	sidechain := truthy(rec["isSidechain"])
	if sidechain && state.Agent == nil {
		state.Agent = strp(subagentType(path))
	}
	input, err := numOrZero(usage, "input_tokens")
	if err != nil {
		return 0, err
	}
	output, err := numOrZero(usage, "output_tokens")
	if err != nil {
		return 0, err
	}
	thinking, err := numOrZero(details, "thinking_tokens")
	if err != nil {
		return 0, err
	}
	effort, err := strField(rec, "effort")
	if err != nil {
		return 0, err
	}
	tier := "standard"
	if strOr(usage, "speed") == "fast" {
		tier = "fast"
	}
	agent := strp(mainAgent)
	if sidechain {
		agent = state.Agent
	}
	ev := &Event{
		// 同一条消息按内容块拆成多行，子代理文件里也会再出现，按消息 id 只计一次
		Ukey:               msgID,
		Ts:                 ts,
		File:               name,
		Model:              strp(modelBracketRe.ReplaceAllString(model, "")),
		Tier:               tier,
		InputTokens:        int64(input) + read + write5m + write1h,
		CachedInputTokens:  read,
		CacheWrite5mTokens: write5m,
		CacheWrite1hTokens: write1h,
		OutputTokens:       int64(output),
		ReasoningTokens:    int64(thinking),
		LimitID:            strp(weekLimitID),
		Agent:              agent,
		Effort:             effort,
	}
	return addClaudeEvent(ctx, rec, msgID, ts, ev)
}

// claudeCacheTokens 从 usage 取缓存读取、1 小时缓存写入与 5 分钟缓存写入的 token，小数向零截断。
// cache_creation 没有 5 分钟分项时，5 分钟写入取 cache_creation_input_tokens 减去 1 小时部分，不低于 0。
func claudeCacheTokens(usage map[string]any) (read, write1h, write5m int64, err error) {
	readN, err := numOrZero(usage, "cache_read_input_tokens")
	if err != nil {
		return 0, 0, 0, err
	}
	written, err := numOrZero(usage, "cache_creation_input_tokens")
	if err != nil {
		return 0, 0, 0, err
	}
	split, err := objOrEmpty(usage, "cache_creation")
	if err != nil {
		return 0, 0, 0, err
	}
	write1hN, err := numOrZero(split, "ephemeral_1h_input_tokens")
	if err != nil {
		return 0, 0, 0, err
	}
	write5mN, has5m, err := numField(split, "ephemeral_5m_input_tokens")
	if err != nil {
		return 0, 0, 0, err
	}
	read, write1h, write5m = int64(readN), int64(write1hN), int64(write5mN)
	if !has5m {
		write5m = max(int64(written)-write1h, 0)
	}
	return read, write1h, write5m, nil
}

// addClaudeEvent 把一条请求记到所属账号的来源下，返回新增的请求数。
// 同一消息已记在某账号下时沿用该账号，否则按会话登记定归属；
// 定不了（resume 复制来的旧消息）的放进 ctx.claudeDeferred，由 settleDeferred 在本轮扫完后再定。
func addClaudeEvent(ctx *Context, rec map[string]any, msgID string, ts float64, ev *Event) (int, error) {
	account, err := recordedAccount(ctx, msgID)
	if err != nil {
		return 0, err
	}
	if account == nil {
		var sessionID string
		if v, ok := rec["sessionId"]; ok && v != nil {
			sessionID = jsonText(v)
		}
		account = accountOf(ctx, sessionID, ts)
	}
	if account == nil {
		ctx.claudeDeferred = append(ctx.claudeDeferred, ev)
		return 0, nil
	}
	added, err := ctx.AddEvent(accountSource(*account), ev)
	if err != nil {
		return 0, err
	}
	if added {
		ctx.claudeAdded[*account]++
		return 1, nil
	}
	return 0, nil
}

// subagentType：子代理日志 agent-<id>.jsonl 旁边的 agent-<id>.meta.json 记着 agentType。
// 元数据读不到、不是 JSON、顶层不是对象或没有 agentType 时，都按没有元数据记作 subagent，请求照常计入。
func subagentType(path string) string {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), stem+".meta.json"))
	if err != nil {
		return "subagent"
	}
	v, err := decodeJSON(data)
	if err != nil {
		return "subagent"
	}
	meta, _ := v.(map[string]any)
	if truthy(meta["agentType"]) {
		return jsonText(meta["agentType"])
	}
	return "subagent"
}

// CollectReadings：agent-usage record 把 7 天窗口的读数追加到这个文件（Claude Code 自己不把额度写进文件）。
// 每个账号一个文件，路径见 Args.rateLimitLog。
func (s *claudeSource) CollectReadings(ctx *Context) error {
	path := ctx.Args.rateLimitLog(s.name)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	_, err = ctx.SyncFiles(s, []string{path}, func(string) string { return "@rate-limits" }, s.ingestReadings, nil)
	return err
}

func (s *claudeSource) ingestReadings(ctx *Context, path, name string, offset int64, state *fileState) (int64, int, error) {
	return readJSONL(ctx, s.name, path, offset, func(line []byte) (int, error) {
		rec, err := decodeObject(line)
		if err != nil {
			return 0, err
		}
		ts, err := requireNum(rec, "ts")
		if err != nil {
			return 0, err
		}
		used, err := requireNum(rec, "used_percentage")
		if err != nil {
			return 0, err
		}
		resets, err := requireNum(rec, "resets_at")
		if err != nil {
			return 0, err
		}
		var droppedFrom *float64
		if from, present, err := numField(rec, "dropped_from"); err != nil {
			return 0, err
		} else if present {
			droppedFrom = floatp(from)
		}
		added, err := ctx.AddReading(s.name, weekLimitID, weekMinutes, ts, used, int64(resets), droppedFrom)
		if err != nil || !added {
			return 0, err
		}
		return 1, nil
	})
}

// cloneAs：共用这份会话日志扫描、价格沿用本来源价格表的另一个账号来源。
func (s *claudeSource) cloneAs(name string) *claudeSource {
	own := *s
	own.baseSource.name = name
	own.baseSource.priceFrom = s.name
	return &own
}
