// time 子命令：日志扫描出的各类区间、一轮内每一刻的归类与优先级、长等待单列、按段裁剪、Bash 用途分类与命令行输出。
package main

import (
	"path/filepath"
	"testing"
	"time"
)

var timingBase = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// at 是合成日志里距基准时刻 sec 秒的时间戳。
func at(sec int) string { return timingBase.Add(time.Duration(sec) * time.Second).Format(time.RFC3339) }

func tsAt(sec int) float64 { return float64(timingBase.Unix() + int64(sec)) }

func apiRequest(id string, sec int, model, effort string) obj {
	return obj{"type": "api-request", "id": id, "timestamp": at(sec),
		"params": obj{"model": model, "output_config": obj{"effort": effort}}}
}

func assistantLine(ref string, sec int, blocks ...obj) obj {
	content := make([]any, 0, len(blocks))
	for _, b := range blocks {
		content = append(content, b)
	}
	return obj{"type": "assistant", "requestRef": ref, "timestamp": at(sec),
		"message": obj{"model": "claude-fable-5-2", "content": content}}
}

func toolUse(id, name string, input obj) obj {
	return obj{"type": "tool_use", "id": id, "name": name, "input": input}
}

func toolResult(id string, sec int, source string) obj {
	return obj{"type": "user", "timestamp": at(sec), "permissionDecision": obj{"decision": "accept", "source": source},
		"message": obj{"role": "user", "content": []any{obj{"type": "tool_result", "tool_use_id": id, "content": "ok"}}}}
}

func turnDuration(sec, seconds int) obj {
	return obj{"type": "system", "subtype": "turn_duration", "timestamp": at(sec), "durationMs": seconds * 1000}
}

// writeTimingSession 写一个主会话：门禁命令 30 秒、提问弹窗等用户 100 秒、等后台通知 100 秒、压缩 20 秒，
// 五次模型请求共 60 秒（其中一次带 advisor），首尾各 10 秒空档；一轮的记录是累计的，最后一条盖住 [0, 330]。
func writeTimingSession(t *testing.T, home string) string {
	t.Helper()
	text := obj{"type": "text", "text": "done"}
	withAdvisor := assistantLine("r3", 170, text)
	withAdvisor["message"].(obj)["usage"] = obj{"iterations": []any{obj{"type": "message"}, obj{"type": "advisor_message"}}}
	return writeJSONL(t, filepath.Join(home, "projects", "proj", "s1.jsonl"),
		obj{"type": "api-request-blob", "timestamp": at(5), "hash": "x"},
		apiRequest("r1", 10, "claude-fable-5-2", "high"),
		assistantLine("r1", 20, toolUse("tu1", "Bash", obj{"command": "cd repo && bash scripts/check.sh"})),
		toolResult("tu1", 50, "config"),
		apiRequest("r2", 50, "claude-fable-5-2", "high"),
		assistantLine("r2", 60, toolUse("tu2", "AskUserQuestion", obj{"questions": []any{}})),
		toolResult("tu2", 160, "user_temporary"),
		apiRequest("r3", 160, "claude-fable-5-2", "xhigh"),
		withAdvisor,
		turnDuration(170, 170),
		obj{"type": "queue-operation", "operation": "enqueue", "timestamp": at(270), "content": "<task-notification>\n<task-id>b1</task-id>"},
		apiRequest("r4", 270, "claude-fable-5-2", "high"),
		assistantLine("r4", 280, text),
		turnDuration(280, 280),
		obj{"type": "system", "subtype": "compact_boundary", "timestamp": at(300), "compactMetadata": obj{"durationMs": 20000}},
		apiRequest("r5", 300, "claude-fable-5-2", "high"),
		assistantLine("r5", 320, toolUse("tu3", "Bash", obj{"command": "sleep 600", "run_in_background": true})),
		toolResult("tu3", 321, "config"),
		obj{"type": "assistant", "timestamp": at(325), "message": obj{"model": "<synthetic>", "content": []any{text}}},
		turnDuration(330, 330),
	)
}

// writeTimingSubagent 写一个子代理日志，并在旁边放一份带同样请求与工具调用的复制件（子代理续接时会这样）。
func writeTimingSubagent(t *testing.T, home string) string {
	t.Helper()
	line := assistantLine("q1", 130, toolUse("tu9", "Bash", obj{"command": "until [ -s rc ]; do sleep 5; done"}))
	line["message"].(obj)["usage"] = obj{"iterations": []any{obj{"type": "advisor_message"}}}
	records := []any{apiRequest("q1", 100, "claude-opus-5-5", "medium"), line, toolResult("tu9", 140, "config")}
	writeJSONL(t, filepath.Join(home, "projects", "proj", "s1", "subagents", "agent-a2.jsonl"), records...)
	return writeJSONL(t, filepath.Join(home, "projects", "proj", "s1", "subagents", "agent-a1.jsonl"), records...)
}

func mustScanTiming(t *testing.T, path string) *sessionTiming {
	t.Helper()
	st, err := scanTimingFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestTimingPartsOfOneSession(t *testing.T) {
	st := mustScanTiming(t, writeTimingSession(t, t.TempDir()))
	if st.sub {
		t.Fatal("主会话文件被当成子代理")
	}
	total, long := sweepParts(sessionEvents(st, tsAt(-100), tsAt(1000)))
	want := [partCount]float64{partModel: 60, partTools: 30, partUser: 100, partBackground: 100, partCompaction: 20, partOther: 20}
	if total != want {
		t.Errorf("各类秒数 = %v，期望 %v（顺序 %v）", total, want, partKeys)
	}
	if long != ([partCount]float64{}) {
		t.Errorf("长等待 = %v，期望没有：每段都不到 %d 秒", long, longStretchSeconds)
	}
}

func TestTimingClipsToSegment(t *testing.T) {
	st := mustScanTiming(t, writeTimingSession(t, t.TempDir()))
	total, _ := sweepParts(sessionEvents(st, tsAt(0), tsAt(165)))
	want := [partCount]float64{partModel: 25, partTools: 30, partUser: 100, partOther: 10}
	if total != want {
		t.Errorf("裁到前 165 秒的各类秒数 = %v，期望 %v", total, want)
	}
	rs := &reqStats{groups: map[[2]string]*modelAcc{}}
	rs.add(st.reqs, tsAt(0), tsAt(165))
	if len(rs.durations) != 3 || rs.advisorCalls != 1 {
		t.Errorf("前 165 秒发出的请求 = %d 次、advisor %d 次，期望 3 次、1 次（按发出时刻归段）", len(rs.durations), rs.advisorCalls)
	}
}

func TestTimingLongStretchAndPriority(t *testing.T) {
	turn := func(e float64) []sweepEvent {
		return []sweepEvent{{0, partCount, 1}, {e, partCount, -1}}
	}
	wait := []sweepEvent{{100, partBackground, 1}, {2100, partBackground, -1}}
	sorted := func(groups ...[]sweepEvent) []sweepEvent {
		st := &sessionTiming{}
		for _, g := range groups {
			for i := 0; i+1 < len(g); i += 2 {
				switch g[i].label {
				case partCount:
					st.turns = append(st.turns, span{g[i].t, g[i+1].t})
				case partBackground:
					st.waits = append(st.waits, span{g[i].t, g[i+1].t})
				case partTools:
					st.tools = append(st.tools, &timeTool{name: "Bash", start: g[i].t, end: g[i+1].t, done: true})
				}
			}
		}
		return sessionEvents(st, 0, 1e9)
	}
	total, long := sweepParts(sorted(turn(5000), wait))
	if total[partBackground] != 2000 || long[partBackground] != 2000 {
		t.Errorf("连续等 2000 秒：合计 %v、长等待 %v，期望都是 2000", total[partBackground], long[partBackground])
	}
	tool := []sweepEvent{{500, partTools, 1}, {600, partTools, -1}}
	total, long = sweepParts(sorted(turn(5000), wait, tool))
	if total[partBackground] != 1900 || total[partTools] != 100 || long[partBackground] != 0 {
		t.Errorf("等待中间跑了 100 秒命令：等待 %v、命令 %v、长等待 %v，期望 1900、100、0（命令优先，等待被切成两段，各不足 %d 秒）",
			total[partBackground], total[partTools], long[partBackground], longStretchSeconds)
	}
	total, _ = sweepParts(sorted(turn(50), wait))
	if total[partBackground] != 0 || total[partOther] != 50 {
		t.Errorf("一轮之外的等待不计：等待 %v、其他 %v，期望 0、50", total[partBackground], total[partOther])
	}
}

func TestBashKind(t *testing.T) {
	for command, want := range map[string]string{
		"wt -C /tree land":                         "merge_release",
		"cd repo && git push origin main":          "merge_release",
		"gh pr merge 12 --squash":                  "merge_release",
		"gh release create v1.0.0":                 "merge_release",
		"until [ -s rc ]; do sleep 5; done":        "wait_poll",
		"cd repo && bash scripts/check.sh | tail":  "gate_test",
		"go test ./... -run TestX":                 "gate_test",
		"timeout 60 ssh build-host 'uptime'":       "remote",
		"sed -n '1,20p' main.go; grep -n foo *.go": "other",
	} {
		if got := bashKind(command); got != want {
			t.Errorf("bashKind(%q) = %s，期望 %s", command, got, want)
		}
	}
}

func TestTimeCommandLine(t *testing.T) {
	ctx := newTestContext(t)
	writeTimingSession(t, ctx.Args.ClaudeHome)
	writeTimingSubagent(t, ctx.Args.ClaudeHome)
	out := cliRun(t, ctx, "time", "--since", "2026-10-31T23:00:00Z", "--until", "2026-11-01T01:00:00Z",
		"--split", "2026-11-01T00:02:45Z")
	segs, ok := out["time"].([]any)
	if !ok || len(segs) != 2 {
		t.Fatalf("time = %#v，期望两段", out["time"])
	}
	first, second := segs[0].(map[string]any), segs[1].(map[string]any)
	if first["sessions"] != 1.0 || first["model_calls"] != 3.0 || second["model_calls"] != 2.0 {
		t.Errorf("两段的会话数与模型调用数 = %v/%v、%v，期望 1/3、2", first["sessions"], first["model_calls"], second["model_calls"])
	}
	if adv := first["advisor"].(map[string]any); adv["calls"] != 1.0 {
		t.Errorf("第一段 advisor = %v，期望 1 次", adv)
	}
	parts := first["parts"].([]any)
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		keys = append(keys, p.(map[string]any)["key"].(string))
	}
	if want := []string{"model", "tools", "waiting_background", "waiting_user", "compaction", "other"}; !equalStrings(keys, want) {
		t.Errorf("各类的顺序 = %v，期望 %v", keys, want)
	}
	if share := parts[3].(map[string]any)["share"]; share != roundTo(100.0/165, 4) {
		t.Errorf("第一段等用户占比 = %v，期望 100/165", share)
	}
	bash := first["bash"].([]any)
	if len(bash) != 1 || bash[0].(map[string]any)["kind"] != "gate_test" || bash[0].(map[string]any)["calls"] != 1.0 {
		t.Errorf("第一段 Bash = %v，期望一次门禁与测试（后台启动的不算）", bash)
	}
	sub := first["subagents"].(map[string]any)
	if sub["files"] != 1.0 || sub["calls"] != 1.0 || sub["advisor"].(map[string]any)["calls"] != 1.0 ||
		sub["model_hours"] != roundTo(30.0/3600, 2) || sub["tool_hours"] != roundTo(10.0/3600, 2) {
		t.Errorf("第一段子代理 = %v，期望 1 个文件、1 次请求、1 次 advisor、模型 30 秒、命令 10 秒（复制件不重复计）", sub)
	}
	if _, has := out["sources"].(map[string]any)["claude"]; has {
		t.Error("time 不该给出按来源的报告块")
	}
}

func TestRenderTime(t *testing.T) {
	home := t.TempDir()
	writeTimingSession(t, home)
	segs, warnings := timeReport(home, []float64{tsAt(0), tsAt(330)})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	got := renderTime(segs)
	want := []string{
		"  提问到收工合计 0.1 小时  主会话 1 个（其中单段超过 30 分钟的等待 0.0 小时，不计入下面的占比）",
		"  模型 18%  跑命令 9%  等后台与消息 30%  等用户 30%  压缩 6%  其他 6%",
		"  模型调用 5 次  中位 10.0s  p90 20.0s  advisor 1 次 0.0 小时（占模型 17%）",
		"  模型与档位  fable-5-2/high 4 次 中位 10.0s  fable-5-2/xhigh 1 次 中位 10.0s",
		"  Bash  门禁与测试 0.0 小时（100%）",
		"  子代理 0 个  模型 0.0 小时  命令 0.0 小时  advisor 0 次 0.0 小时（占模型 -）",
	}
	if len(got) != len(want)+1 || !equalStrings(got[1:], want) {
		t.Errorf("文本输出 =\n%s\n期望（首行为段标题）=\n%s", joinLines(got), joinLines(want))
	}
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
