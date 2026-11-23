// Package reading 记 Claude Code 交出的每周额度读数，是 agent-usage record 的实现，也给跑着 Claude Code 的 Go 程序
// （比如 Zeta 的主机端）在进程内直接调用，不必为每条额度起一个 agent-usage 进程。
//
// Claude Code 只把额度交给它的调用方：stream-json 输出里的 rate_limit_event，和 control 请求 get_usage 的应答。
// Parse 认下列写法：
//
//   - stream-json 的一整行：{"type":"rate_limit_event","rate_limit_info":{…}}
//   - 只有 rate_limit_info：{"unifiedWindows":{"seven_day":{"utilization":0.4,"resetsAt":1790442000}},…}，
//     utilization 是 0～1 的比例，resetsAt 是 epoch 秒
//   - get_usage 的应答：{"rate_limits_available":true,"rate_limits":{"seven_day":{"utilization":40,"resets_at":"…"}},…}，
//     utilization 是 0～100 的百分比，resets_at 是 ISO-8601 时间
//
// 只记 7 天窗口。读数追加在数据目录的 rate-limits[.<账号>].jsonl（LogPath），agent-usage 入库时按行读。
package reading

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dzshzx/claude-switch/account"
)

const (
	// PrimaryAccount 是主号：没装 claude-switch、或它没有切换记录时的账号名。
	PrimaryAccount = account.Default
	// resetsJitter：同一个窗口的重置时刻在各次响应之间会差一两秒。
	resetsJitter = 120
	// DropMin：同一窗口里读数比上一条低这么多个百分点才算回落（官方中途清零、重算档位）；
	// 几个对话的响应乱序到达只会差一个点。
	DropMin = 3.0
	// switchTargetsDir 是 claude-switch 放非账号目标设置文件的目录（相对 HOME），每个目标一份 <名字>.json，
	// 见 claude-switch 的 README「Settings targets」。
	switchTargetsDir = ".config/claude-switch/targets"
)

// ErrNotReading 是交来的 JSON 不是认得的额度写法。
var ErrNotReading = errors.New("不是认得的额度 JSON（rate_limit_event、rate_limit_info 或 get_usage 的应答）")

// Reading 是一份 7 天窗口读数：已用百分比（0～100）与重置时刻（epoch 秒）。
type Reading struct {
	Used     float64
	ResetsAt int64
}

// Line 是读数文件里的一行，也是旁边 .state 文件的内容（上一条记下的读数）。
type Line struct {
	TS          int64    `json:"ts"`
	Window      string   `json:"window"`
	Used        float64  `json:"used_percentage"`
	ResetsAt    int64    `json:"resets_at"`
	DroppedFrom *float64 `json:"dropped_from,omitempty"`
}

// Dir 是 home 下的数据目录：环境变量 AGENT_USAGE_DATA_DIR 非空时取它，否则是 home/.local/share/agent-usage。
func Dir(home string) string {
	if d := os.Getenv("AGENT_USAGE_DATA_DIR"); d != "" {
		return d
	}
	return filepath.Join(home, ".local/share/agent-usage")
}

// LogPath 是账号的读数文件：主号 rate-limits.jsonl，其余 rate-limits.<账号>.jsonl。
func LogPath(dir, name string) string {
	if name == PrimaryAccount {
		return filepath.Join(dir, "rate-limits.jsonl")
	}
	return filepath.Join(dir, "rate-limits."+name+".jsonl")
}

// Account 返回 home 下 claude-switch 的当前名字（切换记录，没有时是 PrimaryAccount），以及它是不是非账号目标。
// 用默认配置目录（不设 CLAUDE_CONFIG_DIR）的 Claude Code 进程都属于这个名字。
func Account(home string) (name string, target bool) {
	name = account.Current(home)
	return name, IsTarget(home, name)
}

// IsTarget 报告 name 是不是 home 下 claude-switch 的非账号目标。
func IsTarget(home, name string) bool {
	_, err := os.Stat(filepath.Join(home, switchTargetsDir, name+".json"))
	return err == nil
}

// Parse 从额度 JSON 里取 7 天窗口读数；认得写法但没带 7 天窗口时 ok 为 false，不认得时是 ErrNotReading。
func Parse(data []byte) (r Reading, ok bool, err error) {
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil || top == nil {
		return r, false, ErrNotReading
	}
	if raw, has := top["rate_limit_info"]; has {
		data = raw
		top = nil
		if json.Unmarshal(data, &top) != nil || top == nil {
			return r, false, ErrNotReading
		}
	}
	switch {
	case top["unifiedWindows"] != nil:
		return parseInfo(data)
	case top["rate_limits_available"] != nil:
		return parseUsage(data)
	}
	return r, false, ErrNotReading
}

func parseInfo(data []byte) (r Reading, ok bool, err error) {
	var info struct {
		Windows map[string]*struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *float64 `json:"resetsAt"`
		} `json:"unifiedWindows"`
	}
	if json.Unmarshal(data, &info) != nil {
		return r, false, ErrNotReading
	}
	w := info.Windows["seven_day"]
	if w == nil || w.Utilization == nil || w.ResetsAt == nil {
		return r, false, nil
	}
	return Reading{Used: roundPercent(*w.Utilization * 100), ResetsAt: int64(*w.ResetsAt)}, true, nil
}

func parseUsage(data []byte) (r Reading, ok bool, err error) {
	var u struct {
		Available bool `json:"rate_limits_available"`
		Limits    *struct {
			SevenDay *struct {
				Utilization *float64 `json:"utilization"`
				ResetsAt    string   `json:"resets_at"`
			} `json:"seven_day"`
		} `json:"rate_limits"`
	}
	if json.Unmarshal(data, &u) != nil {
		return r, false, ErrNotReading
	}
	if !u.Available || u.Limits == nil || u.Limits.SevenDay == nil || u.Limits.SevenDay.Utilization == nil {
		return r, false, nil
	}
	at, err := time.Parse(time.RFC3339Nano, u.Limits.SevenDay.ResetsAt)
	if err != nil {
		return r, false, nil
	}
	return Reading{Used: roundPercent(*u.Limits.SevenDay.Utilization), ResetsAt: at.Unix()}, true, nil
}

// roundPercent 去掉比例换算带来的浮点尾巴（0.9×100 = 90.00000000000001）。
func roundPercent(p float64) float64 { return math.Round(p*100) / 100 }

// Next 返回读数 r 该追加的一行；ok 为 false 时不记。last 是上一条记下的读数（零值表示还没有）。
// 记的是：更晚的窗口；同一窗口里更高的百分比；同一窗口里比上一条低 DropMin 个点以上（带 dropped_from）。
// 更早的窗口迟到了、或同一窗口里没变或只小幅回落时不记。
func Next(r Reading, now int64, last Line) (line Line, ok bool) {
	if r.ResetsAt < last.ResetsAt-resetsJitter {
		return line, false
	}
	line = Line{TS: now, Window: "seven_day", Used: r.Used, ResetsAt: r.ResetsAt}
	if r.ResetsAt-last.ResetsAt > resetsJitter {
		return line, true
	}
	line.ResetsAt = last.ResetsAt
	if r.Used > last.Used {
		return line, true
	}
	if r.Used <= last.Used-DropMin {
		from := last.Used
		line.DroppedFrom = &from
		return line, true
	}
	return line, false
}

// Append 按 Next 把读数追加进 dir 下账号 name 的读数文件，并更新旁边的 .state；记了一行时 recorded 为真。
// 几个程序同时记时由 dir 里的 rate-limits.lock 排队（flock），读 .state、追加、写 .state 是一整步。
func Append(dir, name string, r Reading, now time.Time) (recorded bool, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "rate-limits.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	logPath := LogPath(dir, name)
	statePath := strings.TrimSuffix(logPath, ".jsonl") + ".state"
	var last Line
	if data, err := os.ReadFile(statePath); err == nil {
		if json.Unmarshal(data, &last) != nil {
			last = Line{} // 读不懂的 .state 当作没有上一条：多记一行无害
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	line, ok := Next(r, now.Unix(), last)
	if !ok {
		return false, nil
	}
	encoded, err := json.Marshal(line)
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	_, err = f.Write(append(encoded, '\n'))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	return true, writeFileAtomic(statePath, encoded)
}

// Record 是给别的程序用的一步：从额度 JSON 取读数，记到 home 下 claude-switch 当前的账号名下（数据目录见 Dir）。
// 没带 7 天窗口、读数没变、当前是非账号目标时什么都不记；不认得的 JSON 返回 ErrNotReading。
func Record(home string, data []byte, now time.Time) (recorded bool, err error) {
	r, ok, err := Parse(data)
	if err != nil || !ok {
		return false, err
	}
	name, target := Account(home)
	if target {
		return false, nil
	}
	return Append(Dir(home), name, r, now)
}

// writeFileAtomic 先写同目录的临时文件再改名，读的一方不会看到写了一半的内容。
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}
