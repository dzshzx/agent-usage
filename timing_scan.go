// time 子命令的日志扫描：从会话日志里取出每次模型请求、每次工具调用、每轮起止、压缩与「等通知」的时间区间。
// 只读日志文件，不经数据库。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// span 是一个时间区间 [s, e)，epoch 秒。
type span struct{ s, e float64 }

// timeReq 是一次模型请求：发出时刻到最后一块内容写完的时刻。
type timeReq struct {
	id            string
	start, last   float64
	model, effort string
	advisor       bool
	answered      bool
}

// timeTool 是一次工具调用：模型写完调用的时刻到结果返回的时刻。
type timeTool struct {
	id         string
	name, kind string
	start, end float64
	human      bool // 结果要等用户：提问弹窗、计划确认、用户手动批准的命令
	background bool // 后台启动，调用本身立刻返回
	done       bool
}

// sessionTiming 是一个会话文件（主会话或子代理）里的全部时间区间。
type sessionTiming struct {
	sub      bool
	turns    []span // 一轮从提问到收工，取 turn_duration 记录
	compacts []span
	waits    []span // 上一个动作到「后台任务、子代理或别的会话来通知」之间
	reqs     []*timeReq
	tools    []*timeTool
}

// logLine 只声明 time 子命令用到的字段；其余字段解码时跳过。
type logLine struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	Timestamp   string          `json:"timestamp"`
	ID          string          `json:"id"`
	RequestRef  string          `json:"requestRef"`
	Operation   string          `json:"operation"`
	TurnOrigin  string          `json:"turnOrigin"`
	DurationMs  float64         `json:"durationMs"`
	Content     json.RawMessage `json:"content"`
	ToolResult  json.RawMessage `json:"toolUseResult"`
	Params      *logParams      `json:"params"`
	Message     *logMessage     `json:"message"`
	Permission  *logPermission  `json:"permissionDecision"`
	CompactMeta *logCompactMeta `json:"compactMetadata"`
}

type logParams struct {
	Model        string `json:"model"`
	OutputConfig struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
}

type logMessage struct {
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *struct {
		Iterations []struct {
			Type string `json:"type"`
		} `json:"iterations"`
	} `json:"usage"`
}

type logPermission struct {
	Source string `json:"source"`
}

type logCompactMeta struct {
	DurationMs float64 `json:"durationMs"`
}

type logBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
}

type bashInput struct {
	Command    string `json:"command"`
	Background bool   `json:"run_in_background"`
}

// Bash 命令按用途分类，从上到下取第一个匹配的；都不匹配归 other。
var bashKinds = []struct {
	key string
	re  *regexp.Regexp
}{
	{"merge_release", regexp.MustCompile(`(^|[\s;&|(])(wt\s+(-C\s+\S+\s+)?(land|merge)\b|git\s+push\b|gh\s+(pr\s+merge|release\s+create)\b)`)},
	{"wait_poll", regexp.MustCompile(`\bsleep\s+\d|\buntil\b|gh run watch|kill -0|tail -f|(^|[\s;&])wait(\s|$)`)},
	{"gate_test", regexp.MustCompile(`check[\w-]*\.sh|go (test|vet|build)|golangci|pytest|staticcheck|` +
		`flutter (test|analyze|build)|npm (run )?(test|build|lint)|make (check|test|lint)`)},
	{"remote", regexp.MustCompile(`(^|[\s;&|(])(ssh|scp|rsync)\s`)},
}

func bashKind(command string) string {
	for _, k := range bashKinds {
		if k.re.MatchString(command) {
			return k.key
		}
	}
	return "other"
}

// 这几类行体积大且与计时无关，按行首标记直接跳过，不解码。
var skippedLineMarks = [][]byte{
	[]byte(`"type":"api-request-blob"`), []byte(`"type":"api-request-shape"`), []byte(`"type":"file-history`),
}

// 通知类排队消息的开头：后台任务完成、子代理或别的会话来信。
var noticePrefixes = []string{"<task-notification", "<cross-session", "<agent-message", "<teammate-message"}

// waitMinSeconds：上一个动作到通知之间短于它的不算等待。
const waitMinSeconds = 3

type timingScanner struct {
	out     *sessionTiming
	reqs    map[string]*timeReq
	tools   map[string]*timeTool
	lastAct float64
}

func newTimingScanner(sub bool) *timingScanner {
	return &timingScanner{out: &sessionTiming{sub: sub}, reqs: map[string]*timeReq{}, tools: map[string]*timeTool{}}
}

func skippedLine(raw []byte) bool {
	head := raw[:min(len(raw), 160)]
	for _, mark := range skippedLineMarks {
		if bytes.Contains(head, mark) {
			return true
		}
	}
	return false
}

// line 处理一行日志；解码失败或没有时间戳的行跳过。
func (s *timingScanner) line(raw []byte) {
	if skippedLine(raw) {
		return
	}
	var l logLine
	if json.Unmarshal(raw, &l) != nil || l.Timestamp == "" {
		return
	}
	ts, err := parseTs(l.Timestamp)
	if err != nil {
		return
	}
	switch l.Type {
	case "api-request":
		s.request(&l, ts)
	case "assistant":
		s.assistant(&l, ts)
	case "user":
		s.user(&l, ts)
	case "queue-operation":
		if l.Operation == "enqueue" && isNotice(l.Content) {
			s.notice(ts)
		}
	case "system":
		if !s.system(&l, ts) {
			return
		}
	default:
		return
	}
	s.lastAct = ts
}

func isNotice(content json.RawMessage) bool {
	var text string
	if json.Unmarshal(content, &text) != nil {
		return false
	}
	for _, p := range noticePrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

func (s *timingScanner) notice(ts float64) {
	if s.lastAct > 0 && ts-s.lastAct > waitMinSeconds {
		s.out.waits = append(s.out.waits, span{s.lastAct, ts})
	}
}

func (s *timingScanner) request(l *logLine, ts float64) {
	if l.ID == "" {
		return
	}
	r := &timeReq{id: l.ID, start: ts, last: ts}
	if l.Params != nil {
		r.model, r.effort = strings.TrimPrefix(l.Params.Model, "claude-"), l.Params.OutputConfig.Effort
	}
	s.reqs[l.ID] = r
	s.out.reqs = append(s.out.reqs, r)
}

func (s *timingScanner) assistant(l *logLine, ts float64) {
	if l.Message == nil || l.Message.Model == "<synthetic>" {
		return
	}
	if r := s.reqs[l.RequestRef]; r != nil {
		r.last, r.answered = max(r.last, ts), true
		if l.Message.Usage != nil {
			for _, it := range l.Message.Usage.Iterations {
				r.advisor = r.advisor || it.Type == "advisor_message"
			}
		}
	}
	for _, b := range blocksOf(l.Message.Content) {
		if b.Type != "tool_use" || b.ID == "" {
			continue
		}
		t := &timeTool{id: b.ID, name: b.Name, start: ts, human: b.Name == "AskUserQuestion" || b.Name == "ExitPlanMode"}
		if b.Name == "Bash" {
			var in bashInput
			_ = json.Unmarshal(b.Input, &in)
			t.kind, t.background = bashKind(in.Command), in.Background
		}
		s.tools[b.ID] = t
		s.out.tools = append(s.out.tools, t)
	}
}

func (s *timingScanner) user(l *logLine, ts float64) {
	if l.Message == nil {
		return
	}
	results := 0
	for _, b := range blocksOf(l.Message.Content) {
		if b.Type != "tool_result" {
			continue
		}
		results++
		t := s.tools[b.ToolUseID]
		if t == nil {
			continue
		}
		t.end, t.done = ts, true
		t.human = t.human || (l.Permission != nil && strings.HasPrefix(l.Permission.Source, "user"))
		t.background = t.background || bytes.Contains(l.ToolResult, []byte(`"backgroundTaskId"`)) ||
			bytes.Contains(l.ToolResult, []byte(`"isAsync":true`))
	}
	if results == 0 && (l.TurnOrigin == "task_notification" || l.TurnOrigin == "peer") {
		s.notice(ts)
	}
}

// system 记下一轮的起止与压缩；返回这一行是否算一次动作。
func (s *timingScanner) system(l *logLine, ts float64) bool {
	switch l.Subtype {
	case "turn_duration":
		if l.DurationMs > 0 {
			s.out.turns = append(s.out.turns, span{ts - l.DurationMs/1000, ts})
		}
	case "compact_boundary":
		if l.CompactMeta != nil && l.CompactMeta.DurationMs > 0 {
			s.out.compacts = append(s.out.compacts, span{ts - l.CompactMeta.DurationMs/1000, ts})
		}
	default:
		return false
	}
	return true
}

func blocksOf(content json.RawMessage) []logBlock {
	if len(content) == 0 || content[0] != '[' {
		return nil
	}
	var blocks []logBlock
	_ = json.Unmarshal(content, &blocks)
	return blocks
}

// 单行上限：带整段上下文的请求行可达数百 MB。
const maxLogLineBytes = 1 << 30

// scanTimingFile 读一个会话文件；打不开或中途读失败时返回错误，已读到的部分作废。
func scanTimingFile(path string) (*sessionTiming, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := newTimingScanner(strings.Contains(filepath.ToSlash(path), "/subagents/"))
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), maxLogLineBytes)
	for sc.Scan() {
		s.line(sc.Bytes())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return s.out, nil
}

// scanTimings 并行扫描 claudeHome/projects 下修改时间不早于 since 的会话文件，按路径顺序返回；
// 读不了的文件各记一条 warning。同一条请求或工具调用被复制进多个文件时（resume、子代理续接），只留路径靠前的那份。
func scanTimings(claudeHome string, since float64) ([]*sessionTiming, []string) {
	var paths []string
	for _, p := range globRecursive(filepath.Join(claudeHome, "projects"), "*.jsonl") {
		if info, err := os.Stat(p); err == nil && float64(info.ModTime().Unix()) >= since {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	results := make([]*sessionTiming, len(paths))
	errs := make([]error, len(paths))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(runtime.NumCPU(), 8) {
		wg.Go(func() {
			for i := range jobs {
				results[i], errs[i] = scanTimingFile(paths[i])
			}
		})
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	var out []*sessionTiming
	var warnings []string
	for i, r := range results {
		if errs[i] != nil {
			warnings = append(warnings, "time: 读不了 "+paths[i]+": "+errs[i].Error())
			continue
		}
		out = append(out, r)
	}
	dropCopies(out)
	return out, warnings
}

// dropCopies 按请求 id 与工具调用 id 跨文件去重，先出现的留下。
func dropCopies(sessions []*sessionTiming) {
	seenReq, seenTool := map[string]bool{}, map[string]bool{}
	for _, st := range sessions {
		reqs := st.reqs[:0]
		for _, r := range st.reqs {
			if !seenReq[r.id] {
				seenReq[r.id] = true
				reqs = append(reqs, r)
			}
		}
		st.reqs = reqs
		tools := st.tools[:0]
		for _, t := range st.tools {
			if !seenTool[t.id] {
				seenTool[t.id] = true
				tools = append(tools, t)
			}
		}
		st.tools = tools
	}
}
