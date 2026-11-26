// --text 的文本输出：按来源、周期逐段拼行。
package main

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

func fmtTokens(n int64) string {
	for _, u := range []struct {
		unit string
		size float64
	}{{"B", 1e9}, {"M", 1e6}, {"K", 1e3}} {
		if float64(n) >= u.size {
			return strconv.FormatFloat(float64(n)/u.size, 'f', 1, 64) + u.unit
		}
	}
	return strconv.FormatInt(n, 10)
}

// Result 是一次运行的完整输出。
type Result struct {
	GeneratedAt string                  `json:"generated_at"`
	Sources     map[string]*SourceBlock `json:"sources"`        // 键按来源名的字母序输出
	Time        []*TimeSegment          `json:"time,omitempty"` // 仅 time 子命令
	Warnings    []string                `json:"warnings"`
}

// SourceBlock 是一个来源的报告块；字段集合随子命令变化，由 toJSON 按出现顺序输出。
type SourceBlock struct {
	Currency       string
	Symbol         string
	Sync           *SyncStats
	Prices         *PriceState
	PricesTable    []PriceEntry
	Daily          []*DayReport
	Segments       []*SegmentReport
	QuotaReference *QuotaReference
	Cycles         []*CycleReport
	hasQuota       bool
}

func (b *SourceBlock) MarshalJSON() ([]byte, error) {
	m := newOrderedObject()
	m.Set("currency", b.Currency)
	m.Set("symbol", b.Symbol)
	if b.Sync != nil {
		m.Set("sync", b.Sync)
	}
	if b.Prices != nil {
		m.Set("prices", b.Prices)
	}
	if b.PricesTable != nil {
		m.Set("prices_table", b.PricesTable)
	}
	if b.Daily != nil {
		m.Set("daily", b.Daily)
	}
	if b.Segments != nil {
		m.Set("segments", b.Segments)
	}
	if b.hasQuota {
		m.Set("quota_reference", b.QuotaReference)
	}
	if b.Cycles != nil {
		m.Set("cycles", b.Cycles)
	}
	return m.MarshalJSON()
}

func renderText(result *Result) string {
	var lines []string
	for _, name := range slices.Sorted(maps.Keys(result.Sources)) {
		lines = append(lines, renderSource(name, result.Sources[name])...)
	}
	lines = append(lines, renderTime(result.Time)...)
	for _, w := range result.Warnings {
		lines = append(lines, "warning: "+w)
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \t\n\r")
}

// renderDrops：周期内的读数回落，每次一行。
func renderDrops(drops []ReadingDrop) []string {
	lines := make([]string, 0, len(drops))
	for _, d := range drops {
		lines = append(lines, fmt.Sprintf("  周期内回落  %.16s  %g%% → %g%%", d.At, d.From, d.To))
	}
	return lines
}

// renderSource：一个来源的整段：各周期、近期参考总额度、按日趋势、价格表，末尾空一行隔开下一来源。
func renderSource(name string, block *SourceBlock) []string {
	sym := block.Symbol
	lines := []string{fmt.Sprintf("━━ %s (%s)", name, block.Currency)}
	for _, c := range block.Cycles {
		lines = append(lines, renderCycle(c, sym)...)
	}
	if ref := block.QuotaReference; ref != nil {
		lines = append(lines, fmt.Sprintf("  近期参考总额度 %s%.2f（最近 %d 个已用 ≥%d%% 周期的中位数）",
			sym, ref.Median, ref.Cycles, ref.MinUsedPercent))
	}
	lines = append(lines, renderDaily(block.Daily, sym)...)
	lines = append(lines, renderSegments(block.Segments, sym)...)
	lines = append(lines, renderPrices(block.PricesTable)...)
	lines = append(lines, "")
	return lines
}

// renderPrices：价格表每个 (模型, 档位) 一行；带长上下文价的模型在下面多一行，写明输入超过多少 tokens 时按这行计。
func renderPrices(table []PriceEntry) []string {
	const format = "  %-*s%-9s入 %-8g缓存 %-8s出 %-8g"
	price := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return strconv.FormatFloat(*p, 'g', -1, 64)
	}
	var lines []string
	for _, p := range table {
		lines = append(lines, fmt.Sprintf(format, 30, p.Model, p.Tier, p.Input, price(p.CachedInput), p.Output))
		if p.LongInput == nil || p.LongThreshold == nil {
			continue
		}
		output := p.Output
		if p.LongOutput != nil {
			output = *p.LongOutput
		}
		// 「输入超过」四个字各占两格，名字一栏按字数少补四格才与上一行对齐
		over := fmt.Sprintf("  输入超过 %d tokens", *p.LongThreshold)
		lines = append(lines, fmt.Sprintf(format, 26, over, "", *p.LongInput, price(p.LongCachedInput), output))
	}
	return lines
}

func renderCycle(c *CycleReport, sym string) []string {
	state := "已结束"
	if c.Inferred {
		state = "推断周期"
	} else if c.Active {
		state = "进行中"
	}
	used := "无读数"
	if c.UsedPercent != nil {
		used = fmt.Sprintf("已用 %g%%", *c.UsedPercent)
		if c.UsedPercentAt != nil {
			used += fmt.Sprintf("（读数 %s）", strings.Replace(sliceRunes(*c.UsedPercentAt, 5, 16), "T", " ", 1))
		}
	}
	quota := ""
	if q := c.QuotaEstimate; q != nil {
		quota = fmt.Sprintf("  估算总额度 %s%.2f (%s, %d 个读数)", sym, q.Estimate, q.Method, q.Readings)
	}
	lines := []string{
		fmt.Sprintf("  %s  %.16s → %.16s  %s", c.LimitID, c.WindowStartsAt, c.ResetsAt, state),
		fmt.Sprintf("  %s  折算 %s%.2f%s", used, sym, c.Cost, quota),
	}
	lines = append(lines, renderDrops(c.Drops)...)
	for _, m := range c.Models {
		lines = append(lines, renderModel(m, sym))
	}
	if c.Agents != nil {
		lines = append(lines, renderAgents(*c.Agents, c.Cost, sym)...)
	}
	if c.Sessions != nil {
		lines = append(lines, renderSessions(*c.Sessions, sym)...)
	}
	return lines
}

// renderSessions 逐会话一行：起止时刻、子代理数、请求数、token、金额、占周期金额的比例，有额度估算时再给折成的额度百分比。
// 会话名取路径最后一段（会话 id）的前 24 个字符；会话自己的角色不是主会话时在名字后标角色。
func renderSessions(sessions []SessionRow, sym string) []string {
	lines := []string{"    按会话"}
	for _, s := range sessions {
		quota := ""
		if s.QuotaPercent != nil {
			quota = fmt.Sprintf("  ≈%g%% 额度", *s.QuotaPercent)
		}
		shareText := "-"
		if s.Share != nil {
			shareText = fmt.Sprintf("%.0f%%", *s.Share*100)
		}
		tail := s.Session
		if i := strings.LastIndex(tail, "/"); i >= 0 {
			tail = tail[i+1:]
		}
		role := ""
		if s.Agent != nil && *s.Agent != mainAgent {
			role = " " + *s.Agent
		}
		lines = append(lines, fmt.Sprintf("    %-24.24s%s %s → %s%4d 子代理%6d 次  入 %7s  缓存 %7s  出 %7s  %s%10.2f (%s)%s",
			tail, role, sliceRunes(s.FirstRequestAt, 5, 16), sliceRunes(s.LastRequestAt, 11, 16),
			s.Subagents, s.Requests, fmtTokens(s.InputTokens), fmtTokens(s.CachedInputTokens), fmtTokens(s.OutputTokens),
			sym, s.Cost, shareText, quota))
	}
	return lines
}

func orQuestion(p *string) string {
	if p == nil || *p == "" {
		return "?"
	}
	return *p
}

func renderModel(m ModelRow, sym string) string {
	note := ""
	if m.PricedAs != nil && *m.PricedAs != "" {
		note = fmt.Sprintf("  按 %s 估价", *m.PricedAs)
	}
	if m.UnpricedRequests != 0 {
		note += fmt.Sprintf("  %d 次未计价", m.UnpricedRequests)
	}
	return fmt.Sprintf("    %-26.26s %-9s%6d 次  入 %7s  缓存 %7s  出 %7s  %s%10.2f%s",
		orQuestion(m.Model), m.Tier, m.Requests, fmtTokens(m.InputTokens), fmtTokens(m.CachedInputTokens), fmtTokens(m.OutputTokens),
		sym, m.Cost, note)
}

// renderAgents 先一行按角色汇总的金额与占比，再逐行列出角色、模型、effort、档位分组。
func renderAgents(agents []AgentRow, total float64, sym string) []string {
	shares := newOrderedSums()
	for _, a := range agents {
		shares.add(orQuestion(a.Agent), a.Cost)
	}
	keys := append([]string(nil), shares.keys...)
	sortByValueDesc(keys, shares.vals)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := shares.vals[k]
		if total != 0 {
			parts = append(parts, fmt.Sprintf("%s %s%.2f (%.0f%%)", k, sym, v, v/total*100))
		} else {
			parts = append(parts, fmt.Sprintf("%s %s%.2f", k, sym, v))
		}
	}
	lines := []string{"    按角色  " + strings.Join(parts, " · ")}
	for _, a := range agents {
		effort := "-"
		if a.Effort != nil && *a.Effort != "" {
			effort = *a.Effort
		}
		lines = append(lines, fmt.Sprintf("    %-12.12s %-20.20s %-7s %-9s%4d 会话%6d 次  入 %7s  缓存 %7s  出 %7s  %s%10.2f",
			orQuestion(a.Agent), orQuestion(a.Model), effort, a.Tier, a.Sessions, a.Requests,
			fmtTokens(a.InputTokens), fmtTokens(a.CachedInputTokens), fmtTokens(a.OutputTokens), sym, a.Cost))
	}
	return lines
}

// orderedSums 按键累计金额，保持首次出现的顺序。
type orderedSums struct {
	keys []string
	vals map[string]float64
}

func newOrderedSums() *orderedSums { return &orderedSums{vals: map[string]float64{}} }

func (c *orderedSums) add(k string, v float64) {
	if _, ok := c.vals[k]; !ok {
		c.keys = append(c.keys, k)
	}
	c.vals[k] += v
}

func sortByValueDesc(keys []string, vals map[string]float64) {
	for i := 1; i < len(keys); i++ { // 稳定插入排序：相等的金额保持原顺序
		for j := i; j > 0 && vals[keys[j]] > vals[keys[j-1]]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}

func renderDaily(days []*DayReport, sym string) []string {
	if len(days) == 0 {
		return nil
	}
	peak := 0.0
	for _, d := range days {
		peak = maxFloat(peak, d.Cost)
	}
	if peak == 0 {
		peak = 1
	}
	lines := make([]string, 0, len(days))
	for _, d := range days {
		bars := int(math.RoundToEven(d.Cost / peak * 30))
		lines = append(lines, fmt.Sprintf("  %s  %s%10.2f  %6d 次  入 %7s  出 %7s  %s",
			d.Date, sym, d.Cost, d.Requests, fmtTokens(d.InputTokens), fmtTokens(d.OutputTokens), strings.Repeat("█", bars)))
	}
	return lines
}

// sliceRunes 取 s 的第 a 到 b 个字符，s 不够长时截到末尾。
func sliceRunes(s string, a, b int) string {
	r := []rune(s)
	b = min(b, len(r))
	if a > b {
		return ""
	}
	return string(r[a:b])
}

// renderSegments：segments 的逐段汇总，第二段起另起一行与前一段对比。
func renderSegments(segs []*SegmentReport, sym string) []string {
	var lines []string
	pct := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return strconv.FormatFloat(*p*100, 'f', 0, 64) + "%"
	}
	for i, s := range segs {
		clock := func(iso string) string { return strings.Replace(sliceRunes(iso, 5, 16), "T", " ", 1) }
		lines = append(lines, fmt.Sprintf("  段 %d  %s → %s", i+1, clock(s.Start), clock(s.End)))
		head := fmt.Sprintf("    %s%.2f  %d 次", sym, s.Cost, s.Requests)
		if s.CostPerRequest != nil {
			head += fmt.Sprintf("  每次 %.2f¢", *s.CostPerRequest*100)
		}
		head += fmt.Sprintf("  活跃 %d 小时", s.ActiveHours)
		if s.CostPerActiveHour != nil {
			head += fmt.Sprintf("（每小时 %s%.2f、%.0f 次）", sym, *s.CostPerActiveHour, *s.RequestsPerActiveHour)
		}
		lines = append(lines, head)
		if s.Requests == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("    平均上下文 %s  缓存命中 %s  会话 %d 个",
			fmtTokens(int64(*s.AvgContextTokens)), pct(s.CacheHitRatio), s.Sessions))
		p := s.CostParts
		lines = append(lines, fmt.Sprintf("    构成  读缓存 %s  写缓存 %s  输出 %s  新输入 %s",
			pct(share(p.CacheRead, s.Cost)), pct(share(p.CacheWrite, s.Cost)), pct(share(p.Output, s.Cost)),
			pct(share(p.FreshInput, s.Cost))))
		var bs []string
		for _, b := range s.ContextBuckets {
			label := fmt.Sprintf("%dK+", b.MinTokens/1000)
			if b.MaxTokens != nil {
				label = fmt.Sprintf("%d–%dK", b.MinTokens/1000, *b.MaxTokens/1000)
			}
			bs = append(bs, fmt.Sprintf("%s %s/%s", label, pct(b.RequestShare), pct(b.CostShare)))
		}
		lines = append(lines, "    上下文（请求/金额）  "+strings.Join(bs, "  "))
		sub := fmt.Sprintf("    子代理 %d 个  %s%.2f (%s)", s.Subagents, sym, s.SubagentCost, pct(s.SubagentShare))
		if s.CostPerSubagent != nil {
			sub += fmt.Sprintf("  每个 %s%.2f", sym, *s.CostPerSubagent)
		}
		lines = append(lines, sub)
		lines = append(lines, "    按角色  "+renderGroups(s.Agents, sym, false))
		lines = append(lines, "    按 effort  "+renderGroups(s.Efforts, sym, true))
		if i > 0 {
			lines = append(lines, renderSegmentDelta(segs[i-1], s, i, pct))
		}
	}
	return lines
}

func renderGroups(groups []SegmentGroup, sym string, output bool) string {
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		key := "?"
		if g.Key != nil {
			key = *g.Key
		}
		share := "-"
		if g.Share != nil {
			share = strconv.FormatFloat(*g.Share*100, 'f', 0, 64) + "%"
		}
		item := fmt.Sprintf("%s %s%.2f (%s)", key, sym, g.Cost, share)
		if output && g.OutputPerRequest != nil {
			item += fmt.Sprintf(" 出 %.0f/次", *g.OutputPerRequest)
		}
		parts = append(parts, item)
	}
	return strings.Join(parts, " · ")
}

// renderSegmentDelta：本段相对上一段的变化，两段都有值时才列。
func renderSegmentDelta(prev, cur *SegmentReport, i int, pct func(*float64) string) string {
	change := func(a, b *float64) string {
		if a == nil || b == nil || *a == 0 {
			return "-"
		}
		return fmt.Sprintf("%+.0f%%", (*b / *a - 1)*100)
	}
	return fmt.Sprintf("    对段 %d  每次 %s  每活跃小时 %s  平均上下文 %s  子代理占比 %s → %s",
		i, change(prev.CostPerRequest, cur.CostPerRequest), change(prev.CostPerActiveHour, cur.CostPerActiveHour),
		change(prev.AvgContextTokens, cur.AvgContextTokens), pct(prev.SubagentShare), pct(cur.SubagentShare))
}
