// segments 子命令：把一段时间按给定时刻切成几段，逐段给出成本结构、上下文大小、子代理与 effort，
// 用来对比改配置（上下文窗口、子代理派发、档位）前后的花费。
package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// activeHourMinRequests：一个自然小时内请求数达到它才算活跃小时，零星的几次请求不摊薄每小时花费。
const activeHourMinRequests = 20

// contextBucketEdges 是上下文分档的上界（tokens）；最后一档没有上界。
var contextBucketEdges = []int64{100_000, 200_000, 300_000, 400_000, 500_000}

// CostParts 是金额按计价类别的拆分。
type CostParts struct {
	FreshInput float64 `json:"fresh_input"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Output     float64 `json:"output"`
}

func (p *CostParts) add(q *CostParts) {
	p.FreshInput += q.FreshInput
	p.CacheRead += q.CacheRead
	p.CacheWrite += q.CacheWrite
	p.Output += q.Output
}

func (p CostParts) rounded() CostParts {
	return CostParts{roundTo(p.FreshInput, 4), roundTo(p.CacheRead, 4), roundTo(p.CacheWrite, 4), roundTo(p.Output, 4)}
}

// ContextBucket 是一档上下文大小（每次请求的输入 token，含缓存读写）的请求数与金额。
type ContextBucket struct {
	MinTokens    int64    `json:"min_tokens"`
	MaxTokens    *int64   `json:"max_tokens"`
	Requests     int64    `json:"requests"`
	RequestShare *float64 `json:"request_share"`
	Cost         float64  `json:"cost"`
	CostShare    *float64 `json:"cost_share"`
}

// SegmentGroup 是段内按角色或 effort 的一个分组。
type SegmentGroup struct {
	Key              *string  `json:"key"`
	Files            int64    `json:"files"`
	Requests         int64    `json:"requests"`
	Cost             float64  `json:"cost"`
	Share            *float64 `json:"share"`
	OutputPerRequest *float64 `json:"output_per_request"`
}

// SegmentReport 是一个时间段的汇总。
type SegmentReport struct {
	Start                 string          `json:"start"`
	End                   string          `json:"end"`
	Requests              int64           `json:"requests"`
	Cost                  float64         `json:"cost"`
	UnpricedRequests      int64           `json:"unpriced_requests"`
	CostPerRequest        *float64        `json:"cost_per_request"`
	ActiveHours           int64           `json:"active_hours"`
	CostPerActiveHour     *float64        `json:"cost_per_active_hour"`
	RequestsPerActiveHour *float64        `json:"requests_per_active_hour"`
	AvgContextTokens      *float64        `json:"avg_context_tokens"`
	CacheHitRatio         *float64        `json:"cache_hit_ratio"`
	CostParts             CostParts       `json:"cost_parts"`
	ContextBuckets        []ContextBucket `json:"context_buckets"`
	Sessions              int64           `json:"sessions"`
	Subagents             int64           `json:"subagents"`
	SubagentCost          float64         `json:"subagent_cost"`
	SubagentShare         *float64        `json:"subagent_share"`
	CostPerSubagent       *float64        `json:"cost_per_subagent"`
	Agents                []SegmentGroup  `json:"agents"`
	Efforts               []SegmentGroup  `json:"efforts"`
}

// parseLocalTime 认本地时区的 YYYY-MM-DD、YYYY-MM-DD HH:MM[:SS]（日期与时间之间也可用 T），以及带时区的 RFC 3339。
func parseLocalTime(value string) (time.Time, error) {
	v := strings.TrimSpace(value)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	v = strings.Replace(v, "T", " ", 1)
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间 '%s'（用 YYYY-MM-DD、YYYY-MM-DD HH:MM 或 RFC 3339）", value)
}

// segmentBounds 由 --since、--until 与各 --split 得出各段的 [起, 止) 边界（epoch 秒）。
// since 缺省为 until 前 7 天，until 缺省为 now；切点须严格落在 (since, until) 内，重复的切点报错。
func segmentBounds(since, until string, splits []string, now time.Time) ([]float64, error) {
	end := now
	if until != "" {
		t, err := parseLocalTime(until)
		if err != nil {
			return nil, err
		}
		end = t
	}
	start := end.Add(-7 * 24 * time.Hour)
	if since != "" {
		t, err := parseLocalTime(since)
		if err != nil {
			return nil, err
		}
		start = t
	}
	if !start.Before(end) {
		return nil, fmt.Errorf("--since 须早于 --until")
	}
	bounds := []float64{float64(start.Unix())}
	cuts := make([]float64, 0, len(splits))
	for _, s := range splits {
		t, err := parseLocalTime(s)
		if err != nil {
			return nil, err
		}
		if !t.After(start) || !t.Before(end) {
			return nil, fmt.Errorf("切点 %s 不在 --since 与 --until 之间", s)
		}
		cuts = append(cuts, float64(t.Unix()))
	}
	sort.Float64s(cuts)
	for i := 1; i < len(cuts); i++ {
		if cuts[i] == cuts[i-1] {
			return nil, fmt.Errorf("切点重复：%s", isoOf(cuts[i]))
		}
	}
	bounds = append(bounds, cuts...)
	return append(bounds, float64(end.Unix())), nil
}

type groupAcc struct {
	files    map[string]bool
	requests int64
	output   int64
	amount   float64
}

func newGroupAcc() *groupAcc { return &groupAcc{files: map[string]bool{}} }

func (g *groupAcc) add(file string, output int64, amount float64) {
	g.files[file] = true
	g.requests++
	g.output += output
	g.amount += amount
}

// groupRows 把分组按金额降序排好；键为 nil（取不到角色或 effort）的分组照常列出。
func groupRows(groups map[string]*groupAcc, missing *groupAcc, total float64) []SegmentGroup {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]SegmentGroup, 0, len(keys)+1)
	row := func(key *string, g *groupAcc) SegmentGroup {
		return SegmentGroup{
			Key: key, Files: int64(len(g.files)), Requests: g.requests, Cost: roundTo(g.amount, 4),
			Share: share(g.amount, total), OutputPerRequest: floatp(roundTo(float64(g.output)/float64(g.requests), 1)),
		}
	}
	for _, k := range keys {
		out = append(out, row(strp(k), groups[k]))
	}
	if missing.requests > 0 {
		out = append(out, row(nil, missing))
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Cost > out[b].Cost })
	return out
}

// segmentReport 汇总来源在 [start, end) 内的请求。总金额取入库时算好的 cost，与其余报告一致；
// cost_parts 按同一价格版本重新拆分，未定价的请求不进拆分。
func segmentReport(db *DB, book *PriceBook, src Source, start, end float64) (*SegmentReport, error) {
	rows, err := db.Query(
		"SELECT ts, file, model, tier, input_tokens, cached_input_tokens, cache_write_5m_tokens, "+
			"cache_write_1h_tokens, output_tokens, cost, agent, effort FROM events WHERE source=? AND ts>=? AND ts<?",
		src.Name(), start, end)
	if err != nil {
		return nil, err
	}
	rep := &SegmentReport{Start: isoOf(start), End: isoOf(end)}
	var parts CostParts
	var input, cached int64
	hours := map[int64]int64{}
	sessions := map[string]bool{}
	subFiles := map[string]bool{}
	var subAmount float64
	buckets := make([]ContextBucket, len(contextBucketEdges)+1)
	agents, efforts := map[string]*groupAcc{}, map[string]*groupAcc{}
	noAgent, noEffort := newGroupAcc(), newGroupAcc()
	for _, r := range rows {
		ev := &Event{
			Ts: r.Float("ts"), File: r.Str("file"), Model: r.StrP("model"), Tier: r.Str("tier"),
			InputTokens: r.Int("input_tokens"), CachedInputTokens: r.Int("cached_input_tokens"),
			CacheWrite5mTokens: r.Int("cache_write_5m_tokens"), CacheWrite1hTokens: r.Int("cache_write_1h_tokens"),
			OutputTokens: r.Int("output_tokens"),
		}
		amount := 0.0
		if c := r.FloatP("cost"); c != nil {
			amount = *c
			if p := book.Parts(ev); p != nil {
				parts.add(p)
			}
		} else {
			rep.UnpricedRequests++
		}
		rep.Requests++
		rep.Cost += amount
		input += ev.InputTokens
		cached += ev.CachedInputTokens
		hours[int64(ev.Ts)/3600]++
		name, sub := sessionOf(ev.File)
		sessions[name] = true
		if sub {
			subFiles[ev.File] = true
			subAmount += amount
		}
		b := sort.Search(len(contextBucketEdges), func(i int) bool { return ev.InputTokens < contextBucketEdges[i] })
		buckets[b].Requests++
		buckets[b].Cost += amount
		for _, grp := range []struct {
			key     *string
			named   map[string]*groupAcc
			missing *groupAcc
		}{{r.StrP("agent"), agents, noAgent}, {r.StrP("effort"), efforts, noEffort}} {
			if grp.key == nil {
				grp.missing.add(ev.File, ev.OutputTokens, amount)
				continue
			}
			g, ok := grp.named[*grp.key]
			if !ok {
				g = newGroupAcc()
				grp.named[*grp.key] = g
			}
			g.add(ev.File, ev.OutputTokens, amount)
		}
	}
	total := rep.Cost
	fillActiveHours(rep, rows, hours)
	if rep.Requests > 0 {
		rep.CostPerRequest = floatp(roundTo(total/float64(rep.Requests), 6))
		rep.AvgContextTokens = floatp(roundTo(float64(input)/float64(rep.Requests), 0))
	}
	if input > 0 {
		rep.CacheHitRatio = floatp(roundTo(float64(cached)/float64(input), 4))
	}
	rep.CostParts = parts.rounded()
	lower := int64(0)
	for i := range buckets {
		buckets[i].MinTokens = lower
		if i < len(contextBucketEdges) {
			buckets[i].MaxTokens = intp(contextBucketEdges[i])
			lower = contextBucketEdges[i]
		}
		if rep.Requests > 0 {
			buckets[i].RequestShare = floatp(roundTo(float64(buckets[i].Requests)/float64(rep.Requests), 4))
		}
		buckets[i].CostShare = share(buckets[i].Cost, total)
		buckets[i].Cost = roundTo(buckets[i].Cost, 4)
	}
	rep.ContextBuckets = buckets
	rep.Sessions = int64(len(sessions))
	rep.Subagents = int64(len(subFiles))
	rep.SubagentCost = roundTo(subAmount, 4)
	rep.SubagentShare = share(subAmount, total)
	if rep.Subagents > 0 {
		rep.CostPerSubagent = floatp(roundTo(subAmount/float64(rep.Subagents), 4))
	}
	rep.Agents = groupRows(agents, noAgent, total)
	rep.Efforts = groupRows(efforts, noEffort, total)
	rep.Cost = roundTo(total, 4)
	return rep, nil
}

// fillActiveHours 填 rep 的活跃小时数，有活跃小时时再填每活跃小时的金额与请求数。
// hours 是每个自然小时（epoch 秒整除 3600）的请求数，rows 是段内的请求行。
func fillActiveHours(rep *SegmentReport, rows []Row, hours map[int64]int64) {
	for _, n := range hours {
		if n >= activeHourMinRequests {
			rep.ActiveHours++
		}
	}
	var activeRequests int64
	var activeAmount float64
	if rep.ActiveHours > 0 {
		for _, r := range rows {
			if hours[int64(r.Float("ts"))/3600] >= activeHourMinRequests {
				activeRequests++
				if c := r.FloatP("cost"); c != nil {
					activeAmount += *c
				}
			}
		}
		rep.CostPerActiveHour = floatp(roundTo(activeAmount/float64(rep.ActiveHours), 4))
		rep.RequestsPerActiveHour = floatp(roundTo(float64(activeRequests)/float64(rep.ActiveHours), 1))
	}
}

// segmentsReport 按边界逐段汇总。
func segmentsReport(db *DB, book *PriceBook, src Source, bounds []float64) ([]*SegmentReport, error) {
	out := make([]*SegmentReport, 0, len(bounds)-1)
	for i := 0; i+1 < len(bounds); i++ {
		rep, err := segmentReport(db, book, src, bounds[i], bounds[i+1])
		if err != nil {
			return nil, err
		}
		out = append(out, rep)
	}
	return out, nil
}
