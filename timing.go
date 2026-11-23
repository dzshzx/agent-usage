// time 子命令：把一段时间按给定时刻切成几段，逐段给出主会话「从提问到收工」的时间花在哪
// （模型生成、跑命令、等后台与消息、等用户、压缩），以及模型调用、advisor、Bash 用途与子代理的耗时，
// 用来对比改配置前后的等待时间。数据直接取自会话日志，只覆盖日志还在的时段。
package main

import (
	"sort"
)

// longStretchSeconds：单段连续的等待超过它就算长等待（多半是过夜），单列，不计入占比。
const longStretchSeconds = 1800

// 一轮内每一刻归到一类，重叠时取排在前面的。
const (
	partUser = iota
	partCompaction
	partTools
	partModel
	partBackground
	partOther
	partCount
)

var partKeys = [partCount]string{"waiting_user", "compaction", "tools", "model", "waiting_background", "other"}

// 报告里各类的排列顺序。
var partOrder = []int{partModel, partTools, partBackground, partUser, partCompaction, partOther}

// TimePart 是一类时间；Share 按剔除长等待后的合计算。
type TimePart struct {
	Key   string   `json:"key"`
	Hours float64  `json:"hours"`
	Share *float64 `json:"share"`
}

// TimeAdvisor 是含 advisor 的模型请求。
type TimeAdvisor struct {
	Calls        int64    `json:"calls"`
	Hours        float64  `json:"hours"`
	ShareOfModel *float64 `json:"share_of_model"`
}

// TimeModelRow 是一组模型与档位的调用。
type TimeModelRow struct {
	Model         *string `json:"model"`
	Effort        *string `json:"effort"`
	Calls         int64   `json:"calls"`
	Hours         float64 `json:"hours"`
	MedianSeconds float64 `json:"median_seconds"`
}

// TimeBashRow 是一类用途的 Bash 调用。
type TimeBashRow struct {
	Kind  string   `json:"kind"`
	Calls int64    `json:"calls"`
	Hours float64  `json:"hours"`
	Share *float64 `json:"share"`
}

// TimeSubagents 是子代理日志里的耗时；各子代理并行，小时数是相加的。
type TimeSubagents struct {
	Files      int64       `json:"files"`
	Calls      int64       `json:"calls"`
	ModelHours float64     `json:"model_hours"`
	ToolHours  float64     `json:"tool_hours"`
	Advisor    TimeAdvisor `json:"advisor"`
}

// TimeSegment 是一个时间段的汇总。
type TimeSegment struct {
	Start            string         `json:"start"`
	End              string         `json:"end"`
	Sessions         int64          `json:"sessions"`
	TaskHours        float64        `json:"task_hours"`
	LongStretchHours float64        `json:"long_stretch_hours"`
	Parts            []TimePart     `json:"parts"`
	ModelCalls       int64          `json:"model_calls"`
	ModelCallMedian  *float64       `json:"model_call_median_seconds"`
	ModelCallP90     *float64       `json:"model_call_p90_seconds"`
	Advisor          TimeAdvisor    `json:"advisor"`
	Models           []TimeModelRow `json:"models"`
	Bash             []TimeBashRow  `json:"bash"`
	Subagents        TimeSubagents  `json:"subagents"`
}

type sweepEvent struct {
	t     float64
	label int // partCount 表示一轮的起止
	delta int
}

// clipped 把区间裁到 [a, b)；裁完为空时 ok 为假。
func clipped(sp span, a, b float64) (span, bool) {
	s, e := max(sp.s, a), min(sp.e, b)
	return span{s, e}, e > s
}

// sessionEvents 把一个主会话的各类区间裁到 [a, b) 并摊成起止事件。
func sessionEvents(st *sessionTiming, a, b float64) []sweepEvent {
	var ev []sweepEvent
	add := func(label int, sp span) {
		if c, ok := clipped(sp, a, b); ok {
			ev = append(ev, sweepEvent{c.s, label, 1}, sweepEvent{c.e, label, -1})
		}
	}
	for _, sp := range st.turns {
		add(partCount, sp)
	}
	for _, r := range st.reqs {
		if r.answered {
			add(partModel, span{r.start, r.last})
		}
	}
	for _, t := range st.tools {
		if !t.done || t.background {
			continue
		}
		label := partTools
		if t.human {
			label = partUser
		}
		add(label, span{t.start, t.end})
	}
	for _, sp := range st.compacts {
		add(partCompaction, sp)
	}
	for _, sp := range st.waits {
		add(partBackground, sp)
	}
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].t < ev[j].t })
	return ev
}

// sweepParts 沿时间扫一遍事件，把落在某一轮内的每一刻归到优先级最高的那一类；
// 返回各类合计秒数与其中的长等待秒数（等用户、等后台、其他里单段连续超过 longStretchSeconds 的）。
func sweepParts(ev []sweepEvent) (total, long [partCount]float64) {
	var active [partCount + 1]int
	runLabel, runStart, runEnd := -1, 0.0, 0.0
	flush := func() {
		if runLabel < 0 {
			return
		}
		d := runEnd - runStart
		total[runLabel] += d
		if d > longStretchSeconds && (runLabel == partUser || runLabel == partBackground || runLabel == partOther) {
			long[runLabel] += d
		}
	}
	prev := 0.0
	for i, e := range ev {
		if i > 0 && e.t > prev && active[partCount] > 0 {
			label := partOther
			for p := range partOther {
				if active[p] > 0 {
					label = p
					break
				}
			}
			if label != runLabel || prev != runEnd {
				flush()
				runLabel, runStart = label, prev
			}
			runEnd = e.t
		}
		active[e.label] += e.delta
		prev = e.t
	}
	flush()
	return total, long
}

// percentile 取排好序的 values 的第 p 分位（0 到 1）。
func percentile(sorted []float64, p float64) float64 {
	return sorted[min(len(sorted)-1, int(p*float64(len(sorted))))]
}

func shareOf(part, whole float64) *float64 {
	if whole <= 0 {
		return nil
	}
	return floatp(roundTo(part/whole, 4))
}

func hours(seconds float64) float64 { return roundTo(seconds/3600, 2) }

type modelAcc struct {
	model, effort string
	durations     []float64
	seconds       float64
}

// reqStats 汇总起点落在 [a, b) 的模型请求：次数、各请求耗时、合计秒数、advisor，以及按模型与档位的分组。
type reqStats struct {
	durations      []float64
	seconds        float64
	advisorCalls   int64
	advisorSeconds float64
	groups         map[[2]string]*modelAcc
}

func (rs *reqStats) add(reqs []*timeReq, a, b float64) {
	for _, r := range reqs {
		if !r.answered || r.start < a || r.start >= b {
			continue
		}
		d := r.last - r.start
		rs.durations = append(rs.durations, d)
		rs.seconds += d
		if r.advisor {
			rs.advisorCalls++
			rs.advisorSeconds += d
		}
		key := [2]string{r.model, r.effort}
		g := rs.groups[key]
		if g == nil {
			g = &modelAcc{model: r.model, effort: r.effort}
			rs.groups[key] = g
		}
		g.durations = append(g.durations, d)
		g.seconds += d
	}
}

func (rs *reqStats) advisor() TimeAdvisor {
	return TimeAdvisor{Calls: rs.advisorCalls, Hours: hours(rs.advisorSeconds), ShareOfModel: shareOf(rs.advisorSeconds, rs.seconds)}
}

// maxModelRows：按模型与档位的分组只列耗时最多的几组。
const maxModelRows = 6

func (rs *reqStats) modelRows() []TimeModelRow {
	groups := make([]*modelAcc, 0, len(rs.groups))
	for _, g := range rs.groups {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].seconds != groups[j].seconds {
			return groups[i].seconds > groups[j].seconds
		}
		return groups[i].model+"/"+groups[i].effort < groups[j].model+"/"+groups[j].effort
	})
	rows := []TimeModelRow{}
	for _, g := range groups[:min(len(groups), maxModelRows)] {
		rows = append(rows, TimeModelRow{Model: emptyNil(g.model), Effort: emptyNil(g.effort), Calls: int64(len(g.durations)),
			Hours: hours(g.seconds), MedianSeconds: roundTo(median(g.durations), 1)})
	}
	return rows
}

func emptyNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// toolSeconds 是起点落在 [a, b) 的前台工具调用的合计秒数；等用户的不算。byKind 非空时另按 Bash 用途累计秒数与次数。
func toolSeconds(tools []*timeTool, a, b float64, byKind map[string]*[2]float64) float64 {
	sum := 0.0
	for _, t := range tools {
		if !t.done || t.background || t.human || t.start < a || t.start >= b {
			continue
		}
		d := t.end - t.start
		sum += d
		if byKind == nil || t.name != "Bash" {
			continue
		}
		acc := byKind[t.kind]
		if acc == nil {
			acc = &[2]float64{}
			byKind[t.kind] = acc
		}
		acc[0] += d
		acc[1]++
	}
	return sum
}

func bashRows(byKind map[string]*[2]float64) []TimeBashRow {
	total := 0.0
	for _, acc := range byKind {
		total += acc[0]
	}
	rows := []TimeBashRow{}
	for kind, acc := range byKind {
		rows = append(rows, TimeBashRow{Kind: kind, Calls: int64(acc[1]), Hours: hours(acc[0]), Share: shareOf(acc[0], total)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Hours != rows[j].Hours {
			return rows[i].Hours > rows[j].Hours
		}
		return rows[i].Kind < rows[j].Kind
	})
	return rows
}

// timeSegment 汇总 [a, b) 内的各会话。
func timeSegment(sessions []*sessionTiming, a, b float64) *TimeSegment {
	seg := &TimeSegment{Start: isoOf(a), End: isoOf(b)}
	var total, long [partCount]float64
	mainReqs, subReqs := &reqStats{groups: map[[2]string]*modelAcc{}}, &reqStats{groups: map[[2]string]*modelAcc{}}
	byKind := map[string]*[2]float64{}
	subToolSeconds := 0.0
	for _, st := range sessions {
		if st.sub {
			before := len(subReqs.durations)
			subReqs.add(st.reqs, a, b)
			if len(subReqs.durations) > before {
				seg.Subagents.Files++
			}
			subToolSeconds += toolSeconds(st.tools, a, b, nil)
			continue
		}
		t, l := sweepParts(sessionEvents(st, a, b))
		active := 0.0
		for p := range partCount {
			total[p] += t[p]
			long[p] += l[p]
			active += t[p]
		}
		if active > 0 {
			seg.Sessions++
		}
		mainReqs.add(st.reqs, a, b)
		toolSeconds(st.tools, a, b, byKind)
	}
	fillParts(seg, total, long)
	seg.ModelCalls = int64(len(mainReqs.durations))
	if seg.ModelCalls > 0 {
		sorted := append([]float64(nil), mainReqs.durations...)
		sort.Float64s(sorted)
		seg.ModelCallMedian = floatp(roundTo(median(sorted), 1))
		seg.ModelCallP90 = floatp(roundTo(percentile(sorted, 0.9), 1))
	}
	seg.Advisor, seg.Models, seg.Bash = mainReqs.advisor(), mainReqs.modelRows(), bashRows(byKind)
	seg.Subagents.Calls, seg.Subagents.ModelHours = int64(len(subReqs.durations)), hours(subReqs.seconds)
	seg.Subagents.ToolHours, seg.Subagents.Advisor = hours(subToolSeconds), subReqs.advisor()
	return seg
}

// fillParts 写入合计、长等待与各类占比；占比的分母不含长等待。
func fillParts(seg *TimeSegment, total, long [partCount]float64) {
	sum, longSum := 0.0, 0.0
	for p := range partCount {
		sum += total[p]
		longSum += long[p]
	}
	seg.TaskHours, seg.LongStretchHours = hours(sum), hours(longSum)
	seg.Parts = []TimePart{}
	for _, p := range partOrder {
		typical := total[p] - long[p]
		seg.Parts = append(seg.Parts, TimePart{Key: partKeys[p], Hours: hours(typical), Share: shareOf(typical, sum-longSum)})
	}
}

// timeReport 扫描日志并按 bounds 逐段汇总。
func timeReport(claudeHome string, bounds []float64) ([]*TimeSegment, []string) {
	sessions, warnings := scanTimings(claudeHome, bounds[0])
	out := make([]*TimeSegment, 0, len(bounds)-1)
	for i := 0; i+1 < len(bounds); i++ {
		out = append(out, timeSegment(sessions, bounds[i], bounds[i+1]))
	}
	return out, warnings
}
