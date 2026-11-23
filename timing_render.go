// time 子命令的结果组装与 --text 文本输出。
package main

import (
	"fmt"
	"strconv"
	"strings"
)

// timeResult 是 time 子命令的完整输出：不开数据库、不同步、不对齐价格。
func timeResult(args *Args) *Result {
	segs, warnings := timeReport(args.ClaudeHome, args.Bounds)
	if warnings == nil {
		warnings = []string{}
	}
	return &Result{GeneratedAt: isoOf(now()), Sources: map[string]*SourceBlock{}, Time: segs, Warnings: warnings}
}

var partLabels = map[string]string{
	"model": "模型", "tools": "跑命令", "waiting_background": "等后台与消息", "waiting_user": "等用户",
	"compaction": "压缩", "other": "其他",
}

var bashKindLabels = map[string]string{
	"merge_release": "合入与发版", "wait_poll": "等待轮询", "gate_test": "门禁与测试", "remote": "远端", "other": "其他",
}

func percentText(p *float64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatFloat(*p*100, 'f', 0, 64) + "%"
}

func advisorText(a TimeAdvisor) string {
	return fmt.Sprintf("advisor %d 次 %.1f 小时（占模型 %s）", a.Calls, a.Hours, percentText(a.ShareOfModel))
}

// renderTime：time 的逐段汇总；没有段时不输出。
func renderTime(segs []*TimeSegment) []string {
	var lines []string
	clock := func(iso string) string { return strings.Replace(sliceRunes(iso, 5, 16), "T", " ", 1) }
	for i, s := range segs {
		lines = append(lines, fmt.Sprintf("段 %d  %s → %s", i+1, clock(s.Start), clock(s.End)))
		lines = append(lines, fmt.Sprintf("  提问到收工合计 %.1f 小时  主会话 %d 个（其中单段超过 30 分钟的等待 %.1f 小时，不计入下面的占比）",
			s.TaskHours, s.Sessions, s.LongStretchHours))
		parts := make([]string, 0, len(s.Parts))
		for _, p := range s.Parts {
			parts = append(parts, fmt.Sprintf("%s %s", partLabels[p.Key], percentText(p.Share)))
		}
		lines = append(lines, "  "+strings.Join(parts, "  "))
		lines = append(lines, renderTimeModel(s)...)
		if len(s.Bash) > 0 {
			rows := make([]string, 0, len(s.Bash))
			for _, b := range s.Bash {
				rows = append(rows, fmt.Sprintf("%s %.1f 小时（%s）", bashKindLabels[b.Kind], b.Hours, percentText(b.Share)))
			}
			lines = append(lines, "  Bash  "+strings.Join(rows, "  "))
		}
		sub := s.Subagents
		lines = append(lines, fmt.Sprintf("  子代理 %d 个  模型 %.1f 小时  命令 %.1f 小时  %s", sub.Files, sub.ModelHours, sub.ToolHours,
			advisorText(sub.Advisor)))
	}
	return lines
}

// renderTimeModel：主会话模型调用的次数、耗时分位、advisor，以及按模型与档位的分组。
func renderTimeModel(s *TimeSegment) []string {
	if s.ModelCalls == 0 {
		return []string{"  模型调用 0 次"}
	}
	lines := []string{fmt.Sprintf("  模型调用 %d 次  中位 %.1fs  p90 %.1fs  %s", s.ModelCalls, *s.ModelCallMedian, *s.ModelCallP90,
		advisorText(s.Advisor))}
	rows := make([]string, 0, len(s.Models))
	for _, m := range s.Models {
		rows = append(rows, fmt.Sprintf("%s/%s %d 次 中位 %.1fs", orQuestion(m.Model), orQuestion(m.Effort), m.Calls, m.MedianSeconds))
	}
	return append(lines, "  模型与档位  "+strings.Join(rows, "  "))
}
