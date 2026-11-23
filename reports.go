// 汇总：周期划分、周期报告、额度估算与按日趋势。按模型、角色、会话的拆分在 breakdown.go。
package main

import (
	"database/sql"
	"math"
	"sort"
	"time"
)

// QuotaEstimate 是一个周期的总额度估算。
type QuotaEstimate struct {
	Readings   int       `json:"readings"`
	Ticks      int       `json:"ticks"`
	Ratio      float64   `json:"ratio"`
	RatioRange []float64 `json:"ratio_range"`
	Delta      *float64  `json:"delta"`
	Method     string    `json:"method"`
	Estimate   float64   `json:"estimate"`
}

// readingPoint 是 (used_percent, 读数时刻的周期累计金额)；drop 为真表示这条读数是周期内的回落。
type readingPoint struct {
	used float64
	cost float64
	drop bool
}

// quotaSegment 是两次回落之间的一段读数：base 是段的起点（第一段是周期起点，金额与百分比都从零算；
// 之后各段是那条回落读数），last 是段内最后一条，ticks 是段内每次读数上跳那一刻。
type quotaSegment struct {
	base, last readingPoint
	ticks      []readingPoint
}

// splitSegments 在回落读数处把按时间排序的读数切段。首条就是回落时第一段为空，不贡献任何增量。
func splitSegments(points []readingPoint) []quotaSegment {
	segs := []quotaSegment{{}}
	top := points[0].used
	for _, p := range points {
		switch {
		case p.drop:
			segs = append(segs, quotaSegment{base: p})
			top = p.used
		case p.used > top:
			segs[len(segs)-1].ticks = append(segs[len(segs)-1].ticks, p)
			top = p.used
		}
		segs[len(segs)-1].last = p
	}
	return segs
}

func allIntegers(points []readingPoint) bool {
	for _, p := range points {
		if p.used != math.Trunc(p.used) {
			return false
		}
	}
	return true
}

// estimateQuota 的 points 按时间排序。
//
// ratio 用读数直接相除；delta 用两次读数上跳之间的金额增量，能抵消周期里
// 没被本机日志覆盖的那部分用量（例如读数记录是周期中途才装上的）。
// 周期内的回落（官方中途清零或重算档位）把读数切成几段：ratio 取各段金额增量之和
// 除以各段百分比增量之和，delta 的上跳只在段内配对；跨过回落的那段金额两边都不计。
// 没有回落时只有一段，两种口径与按整个周期算相同。
func estimateQuota(points []readingPoint) *QuotaEstimate {
	if len(points) == 0 {
		return nil
	}
	segs := splitSegments(points)
	var cost, used, tickCost, tickUsed float64
	ticks := 0
	for _, s := range segs {
		ticks += len(s.ticks)
		if s.last.used > s.base.used {
			cost += s.last.cost - s.base.cost
			used += s.last.used - s.base.used
		}
		if len(s.ticks) >= 2 {
			tickCost += s.ticks[len(s.ticks)-1].cost - s.ticks[0].cost
			tickUsed += s.ticks[len(s.ticks)-1].used - s.ticks[0].used
		}
	}
	if used <= 0 {
		return nil
	}
	out := &QuotaEstimate{Readings: len(points), Ticks: ticks, Ratio: roundTo(cost/used*100, 2)}
	if len(segs) == 1 && allIntegers(points) {
		// 读数按整数取整：真实占比落在 [used, used+1) 内；有回落时起点不是零，不给区间
		out.RatioRange = []float64{roundTo(cost/(used+1)*100, 2), out.Ratio}
	}
	if tickUsed >= 3 {
		out.Delta = floatp(roundTo(tickCost/tickUsed*100, 2))
	}
	out.Method = "ratio"
	out.Estimate = out.Ratio
	if out.Delta != nil {
		out.Method = "delta"
		out.Estimate = *out.Delta
	}
	return out
}

// Cycle 是一个限额周期的边界。
type Cycle struct {
	LimitID  string
	ResetsAt int64
	Window   int64
	Begin    float64
	End      float64
	Inferred bool
}

// listCycles 返回来源的全部周期，新的在前。有读数的周期来自 readings；没有读数覆盖的请求落进按 7 天推断的周期。
func listCycles(db *DB, src Source) ([]Cycle, error) {
	// 请求里的限额名沿 events_cycle 索引跳着取（每个值一次索引查找），DISTINCT 要把该来源的请求全扫一遍
	rows, err := db.Query(`WITH RECURSIVE s(lid) AS (
    SELECT MIN(limit_id) FROM events WHERE source=?1
    UNION ALL SELECT (SELECT MIN(limit_id) FROM events WHERE source=?1 AND limit_id > s.lid) FROM s WHERE s.lid IS NOT NULL)
SELECT lid FROM s WHERE lid IS NOT NULL
UNION SELECT DISTINCT limit_id FROM readings WHERE source=?1
ORDER BY 1`, src.Name())
	if err != nil {
		return nil, err
	}
	limitIDs := make([]string, len(rows))
	for i, r := range rows {
		limitIDs[i] = r.Str("lid")
	}
	var cycles []Cycle
	for _, lid := range limitIDs {
		rows, err := db.Query(
			"SELECT resets_at, MAX(window_minutes) window, MIN(ts) first, MAX(ts) last FROM readings "+
				"WHERE source=? AND limit_id=? GROUP BY resets_at ORDER BY resets_at", src.Name(), lid)
		if err != nil {
			return nil, err
		}
		known := make([]Cycle, 0, len(rows))
		for i, r := range rows {
			c := Cycle{LimitID: lid, ResetsAt: r.Int("resets_at"), Window: r.Int("window")}
			c.Begin = float64(c.ResetsAt - c.Window*60)
			c.End = float64(c.ResetsAt)
			if i > 0 {
				prev := &known[i-1]
				prev.End = math.Min(prev.End, math.Max(c.Begin, prev.Begin))
				c.Begin = math.Max(c.Begin, prev.End)
			}
			known = append(known, c)
		}
		inferred, err := inferredCycles(db, src, lid, known)
		if err != nil {
			return nil, err
		}
		cycles = append(cycles, known...)
		cycles = append(cycles, inferred...)
	}
	sort.SliceStable(cycles, func(i, j int) bool { return cycles[i].ResetsAt > cycles[j].ResetsAt })
	return cycles, nil
}

type gap struct {
	lo, hi   float64
	backward bool // 是否从止端往回切
}

func inferredCycles(db *DB, src Source, limitID string, known []Cycle) ([]Cycle, error) {
	// MIN 与 MAX 分成两个子查询才各自只取索引的一端，写在一起会把这些请求全扫一遍
	const nullCycle = "FROM events WHERE source=?1 AND limit_id=?2 AND cycle_resets IS NULL"
	span, err := db.QueryRow("SELECT (SELECT MIN(ts) "+nullCycle+") lo, (SELECT MAX(ts) "+nullCycle+") hi", src.Name(), limitID)
	if err != nil || span == nil || span.IsNull("lo") {
		return nil, err
	}
	step := float64(weekMinutes * 60)
	var gaps []gap
	edge := span.Float("lo")
	for _, c := range known {
		if c.Begin > edge {
			gaps = append(gaps, gap{edge, c.Begin, true})
		}
		edge = math.Max(edge, c.End)
	}
	if span.Float("hi") >= edge {
		gaps = append(gaps, gap{edge, span.Float("hi") + 1, len(known) == 0})
	}
	var out []Cycle
	for _, g := range gaps {
		for _, b := range splitGap(g, step) {
			hit, err := db.QueryRow(
				"SELECT 1 FROM events WHERE source=? AND limit_id=? AND cycle_resets IS NULL AND ts>=? AND ts<? LIMIT 1",
				src.Name(), limitID, b[0], b[1])
			if err != nil {
				return nil, err
			}
			if hit != nil {
				out = append(out, Cycle{LimitID: limitID, ResetsAt: int64(b[1]), Window: weekMinutes, Begin: b[0], End: b[1], Inferred: true})
			}
		}
	}
	return out, nil
}

// splitGap 把一段空档切成若干 7 天区间，backward 时从止端往回切。
func splitGap(g gap, step float64) [][2]float64 {
	var bounds [][2]float64
	lo, hi := g.lo, g.hi
	if g.backward {
		for hi > lo {
			bounds = append(bounds, [2]float64{math.Max(lo, hi-step), hi})
			hi -= step
		}
	} else {
		for lo < hi {
			bounds = append(bounds, [2]float64{lo, lo + step})
			lo += step
		}
	}
	return bounds
}

// memberSQL 是周期内全部请求的子查询：日志自带读数的请求在入库时已定好周期；其余按时间落进周期区间。
// 拆成两段各走 events_cycle 索引，写成 OR 会退化为按来源全表扫描。参数见 memberArgs。
const memberSQL = "(SELECT * FROM events WHERE source=? AND limit_id=? AND cycle_resets=? UNION ALL " +
	"SELECT * FROM events WHERE source=? AND limit_id=? AND cycle_resets IS NULL AND ts>=? AND ts<?)"

func memberArgs(src Source, c Cycle) []any {
	var resets any
	if !c.Inferred {
		resets = c.ResetsAt
	}
	return []any{src.Name(), c.LimitID, resets, src.Name(), c.LimitID, c.Begin, c.End}
}

// CycleReport 是一个限额周期的报告。
type CycleReport struct {
	LimitID          string         `json:"limit_id"`
	WindowMinutes    int64          `json:"window_minutes"`
	Plan             *string        `json:"plan"`
	Active           bool           `json:"active"`
	Inferred         bool           `json:"inferred"`
	WindowStartsAt   string         `json:"window_starts_at"`
	ResetsAt         string         `json:"resets_at"`
	FirstRequestAt   *string        `json:"first_request_at"`
	LastRequestAt    *string        `json:"last_request_at"`
	UsedPercent      *float64       `json:"used_percent"`
	UsedPercentAt    *string        `json:"used_percent_at"`
	Requests         int            `json:"requests"`
	Cost             float64        `json:"cost"`
	UnpricedRequests int64          `json:"unpriced_requests"`
	QuotaEstimate    *QuotaEstimate `json:"quota_estimate"`
	Drops            []ReadingDrop  `json:"drops,omitempty"`
	Models           []ModelRow     `json:"models"`
	Agents           *[]AgentRow    `json:"agents,omitempty"`
	Sessions         *[]SessionRow  `json:"sessions,omitempty"`
}

// costEvent 是一次请求的时刻与金额；缺价的请求金额记 0，累加时与跳过相同。
type costEvent struct {
	ts, cost float64
}

// cycleQuota 是一个周期的读数、按时间排好的请求金额，以及据此算出的额度估算；参考额度只用到这一部分。
type cycleQuota struct {
	readings []Row
	events   []costEvent
	total    float64
	used     *float64 // 最后一条读数的已用百分比
	estimate *QuotaEstimate
}

func loadCycleQuota(db *DB, src Source, c Cycle) (*cycleQuota, error) {
	q := &cycleQuota{}
	// 一个周期几万行：逐行按列取值，不经 Query 给每行建 map
	err := db.Each("SELECT ts, cost FROM "+memberSQL, memberArgs(src, c), func(scan func(...any) error) error {
		var ts float64
		var cost sql.NullFloat64
		if err := scan(&ts, &cost); err != nil {
			return err
		}
		q.total += cost.Float64
		q.events = append(q.events, costEvent{ts, cost.Float64})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(q.events, func(i, j int) bool { return q.events[i].ts < q.events[j].ts })
	if q.readings, err = cycleReadings(db, src, c); err != nil {
		return nil, err
	}
	if len(q.readings) > 0 {
		q.used = floatp(q.readings[len(q.readings)-1].Float("used_percent"))
	}
	q.estimate = estimateQuota(readingPoints(q.readings, q.events))
	return q, nil
}

// active：周期还没结束（提前重置后，旧周期的实际终点是下一周期的起点）；推断出来的周期不算。
func (c Cycle) active() bool {
	return math.Min(float64(c.ResetsAt), c.End) > now() && !c.Inferred
}

// cycleReport 是周期 c 的完整报告；q 是已经读好的这个周期的读数与请求金额（见 loadCycleQuota）。
func cycleReport(db *DB, ctx *Context, src Source, c Cycle, q *cycleQuota, byAgent, bySession bool) (*CycleReport, error) {
	key := memberArgs(src, c)
	book, err := ctx.Book(src, false)
	if err != nil {
		return nil, err
	}
	models, err := modelBreakdown(db, book, key)
	if err != nil {
		return nil, err
	}
	readings, events, total := q.readings, q.events, q.total
	report := &CycleReport{
		LimitID:       c.LimitID,
		WindowMinutes: c.Window,
		Plan:          lastPlan(readings),
		Active:        c.active(),
		Inferred:      c.Inferred,
		ResetsAt:      isoOf(float64(c.ResetsAt)),
		Requests:      len(events),
		Cost:          roundTo(total, 4),
		QuotaEstimate: q.estimate,
		UsedPercent:   q.used,
		Drops:         readingDrops(readings),
		Models:        models,
	}
	if c.Inferred {
		report.WindowStartsAt = isoOf(c.Begin)
	} else {
		report.WindowStartsAt = isoOf(float64(c.ResetsAt - c.Window*60))
	}
	if len(events) > 0 {
		report.FirstRequestAt = strp(isoOf(events[0].ts))
		report.LastRequestAt = strp(isoOf(events[len(events)-1].ts))
	}
	if len(readings) > 0 {
		report.UsedPercentAt = strp(isoOf(readings[len(readings)-1].Float("ts")))
	}
	for _, m := range models {
		report.UnpricedRequests += m.UnpricedRequests
	}
	if byAgent {
		agents, err := agentBreakdown(db, key, total)
		if err != nil {
			return nil, err
		}
		report.Agents = &agents
	}
	if bySession {
		var quota *float64
		if report.QuotaEstimate != nil {
			quota = floatp(report.QuotaEstimate.Estimate)
		}
		sessions, err := sessionBreakdown(db, key, total, quota)
		if err != nil {
			return nil, err
		}
		report.Sessions = &sessions
	}
	return report, nil
}

func lastPlan(readings []Row) *string {
	for i := len(readings) - 1; i >= 0; i-- {
		if p := readings[i].StrP("plan"); p != nil && *p != "" {
			return p
		}
	}
	return nil
}

// cycleReadings 返回周期内的额度读数，按时间排序；推断出来的周期没有读数。
func cycleReadings(db *DB, src Source, c Cycle) ([]Row, error) {
	if c.Inferred {
		return nil, nil
	}
	return db.Query(
		"SELECT ts, used_percent, plan, dropped_from FROM readings WHERE source=? AND limit_id=? AND resets_at=? ORDER BY ts",
		src.Name(), c.LimitID, c.ResetsAt)
}

// readingPoints 给每条额度读数配上读数时刻为止的周期累计金额，供 estimateQuota 使用；两者都按时间排好序。
func readingPoints(readings []Row, events []costEvent) []readingPoint {
	var points []readingPoint
	running, i := 0.0, 0
	for _, rd := range readings {
		for i < len(events) && events[i].ts <= rd.Float("ts") {
			running += events[i].cost
			i++
		}
		points = append(points, readingPoint{rd.Float("used_percent"), running, !rd.IsNull("dropped_from")})
	}
	return points
}

// ReadingDrop 是周期内的一次读数回落：at 是回落后第一条读数的时刻，from、to 是回落前后的百分比。
type ReadingDrop struct {
	At   string  `json:"at"`
	From float64 `json:"from"`
	To   float64 `json:"to"`
}

// readingDrops 列出周期内带回落标记的读数，按时间排序；没有回落时为 nil（JSON 里不出现 drops）。
func readingDrops(readings []Row) []ReadingDrop {
	var drops []ReadingDrop
	for _, rd := range readings {
		if from := rd.FloatP("dropped_from"); from != nil {
			drops = append(drops, ReadingDrop{At: isoOf(rd.Float("ts")), From: *from, To: rd.Float("used_percent")})
		}
	}
	return drops
}

// QuotaReference 是近期参考总额度。
type QuotaReference struct {
	Median         float64 `json:"median"`
	Cycles         int     `json:"cycles"`
	MinUsedPercent int     `json:"min_used_percent"`
}

// reference：有额度估算、用得够多（已用百分比够高，估算才稳）的周期才进参考，返回它的估算；推断周期没有读数，不进参考。
func (q *cycleQuota) reference() (float64, bool) {
	if q.estimate == nil || q.used == nil || *q.used < referenceMinUsedPercent {
		return 0, false
	}
	return q.estimate.Estimate, true
}

// quotaReference 取各够格周期估算的中位数；一个都没有时为 nil。
func quotaReference(picked []float64) *QuotaReference {
	if len(picked) == 0 {
		return nil
	}
	return &QuotaReference{Median: roundTo(median(picked), 2), Cycles: len(picked), MinUsedPercent: referenceMinUsedPercent}
}

// DayReport 是按日趋势的一天。
type DayReport struct {
	Date              string         `json:"date"`
	Requests          int64          `json:"requests"`
	InputTokens       int64          `json:"input_tokens"`
	CachedInputTokens int64          `json:"cached_input_tokens"`
	OutputTokens      int64          `json:"output_tokens"`
	Cost              float64        `json:"cost"`
	Models            *orderedObject `json:"models"` // 模型 → 当天金额，按金额从高到低
}

func dailyReport(db *DB, src Source, days int) ([]*DayReport, error) {
	n := time.Now().Local()
	start := float64(time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()).Unix())
	start -= float64(days-1) * 86400
	rows, err := db.Query(
		"SELECT date(ts, 'unixepoch', 'localtime') day, model, COUNT(*) n, SUM(input_tokens) i, "+
			"SUM(cached_input_tokens) c, SUM(output_tokens) o, SUM(cost) amount FROM events "+
			"WHERE source=? AND ts>=? GROUP BY day, model ORDER BY day, amount DESC", src.Name(), start)
	if err != nil {
		return nil, err
	}
	out := make([]*DayReport, 0)
	index := map[string]*DayReport{}
	for _, r := range rows {
		day := r.Str("day")
		d, ok := index[day]
		if !ok {
			d = &DayReport{Date: day, Models: newOrderedObject()}
			index[day] = d
			out = append(out, d)
		}
		amount := r.Float("amount")
		d.Requests += r.Int("n")
		d.InputTokens += r.Int("i")
		d.CachedInputTokens += r.Int("c")
		d.OutputTokens += r.Int("o")
		d.Cost = roundTo(d.Cost+amount, 4)
		model := "(未知模型)"
		if m := r.StrP("model"); m != nil {
			model = *m
		}
		d.Models.Set(model, roundTo(amount, 4))
	}
	return out, nil
}

func sortStrings(s []string) { sort.Strings(s) }
