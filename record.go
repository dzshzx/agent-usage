// record：把 Claude Code 交出的每周额度读数记进读数文件，供额度估算用。
//
// Claude Code 只把额度交给它的调用方：stream-json 输出里的 rate_limit_event，和 control 请求 get_usage 的应答。
// 跑着 Claude Code 的程序（比如 Zeta 的主机端）每拿到一份就交给 agent-usage record，从 stdin 传一个 JSON 对象，
// 下列写法都认：
//
//   - stream-json 的一整行：{"type":"rate_limit_event","rate_limit_info":{…}}
//   - 只有 rate_limit_info：{"unifiedWindows":{"seven_day":{"utilization":0.4,"resetsAt":1790442000}},…}，
//     utilization 是 0～1 的比例，resetsAt 是 epoch 秒
//   - get_usage 的应答：{"rate_limits_available":true,"rate_limits":{"seven_day":{"utilization":40,"resets_at":"…"}},…}，
//     utilization 是 0～100 的百分比，resets_at 是 ISO-8601 时间
//
// 只记 7 天窗口。读数属于当前账号（claude-switch 的切换记录，见 account.Name），可用 --account 指定；
// 当前是 claude-switch 的非账号目标时没有额度，什么都不记。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// readingResetsJitter：同一个窗口的重置时刻在各次响应之间会差一两秒。
	readingResetsJitter = 120
	// readingDropMin：同一窗口里读数比上一条低这么多个百分点才算回落（官方中途清零、重算档位）；
	// 几个对话的响应乱序到达只会差一个点。
	readingDropMin = 3.0
)

// errNotReading 是 stdin 里的 JSON 不是认得的额度写法。
var errNotReading = errors.New("stdin 不是认得的额度 JSON（rate_limit_event、rate_limit_info 或 get_usage 的应答）")

// weekReading 是一份 7 天窗口读数：已用百分比（0～100）与重置时刻（epoch 秒）。
type weekReading struct {
	used     float64
	resetsAt int64
}

// readingLine 是读数文件里的一行，也是旁边 .state 文件的内容（上一条记下的读数）。
type readingLine struct {
	TS          int64    `json:"ts"`
	Window      string   `json:"window"`
	Used        float64  `json:"used_percentage"`
	ResetsAt    int64    `json:"resets_at"`
	DroppedFrom *float64 `json:"dropped_from,omitempty"`
}

// parseReading 从 stdin 的 JSON 里取 7 天窗口读数；认得写法但没带 7 天窗口时 ok 为 false。
func parseReading(data []byte) (r weekReading, ok bool, err error) {
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil || top == nil {
		return r, false, errNotReading
	}
	if raw, has := top["rate_limit_info"]; has {
		if json.Unmarshal(raw, &top) != nil || top == nil {
			return r, false, errNotReading
		}
	}
	switch {
	case top["unifiedWindows"] != nil:
		var info struct {
			Windows map[string]*struct {
				Utilization *float64 `json:"utilization"`
				ResetsAt    *float64 `json:"resetsAt"`
			} `json:"unifiedWindows"`
		}
		if err := json.Unmarshal(mustRaw(top), &info); err != nil {
			return r, false, errNotReading
		}
		w := info.Windows["seven_day"]
		if w == nil || w.Utilization == nil || w.ResetsAt == nil {
			return r, false, nil
		}
		return weekReading{used: roundPercent(*w.Utilization * 100), resetsAt: int64(*w.ResetsAt)}, true, nil
	case top["rate_limits_available"] != nil:
		var u struct {
			Available bool `json:"rate_limits_available"`
			Limits    *struct {
				SevenDay *struct {
					Utilization *float64 `json:"utilization"`
					ResetsAt    string   `json:"resets_at"`
				} `json:"seven_day"`
			} `json:"rate_limits"`
		}
		if err := json.Unmarshal(mustRaw(top), &u); err != nil {
			return r, false, errNotReading
		}
		if !u.Available || u.Limits == nil || u.Limits.SevenDay == nil || u.Limits.SevenDay.Utilization == nil {
			return r, false, nil
		}
		at, err := time.Parse(time.RFC3339Nano, u.Limits.SevenDay.ResetsAt)
		if err != nil {
			return r, false, nil
		}
		return weekReading{used: roundPercent(*u.Limits.SevenDay.Utilization), resetsAt: at.Unix()}, true, nil
	}
	return r, false, errNotReading
}

func mustRaw(m map[string]json.RawMessage) []byte {
	out, _ := json.Marshal(m)
	return out
}

// roundPercent 去掉比例换算带来的浮点尾巴（0.9×100 = 90.00000000000001）。
func roundPercent(p float64) float64 { return math.Round(p*100) / 100 }

// nextReading 返回这份读数该追加的一行；ok 为 false 时不记。last 是上一条记下的读数（零值表示还没有）。
// 记的是：更晚的窗口；同一窗口里更高的百分比；同一窗口里比上一条低 readingDropMin 个点以上（带 dropped_from）。
// 更早的窗口迟到了、或同一窗口里没变或只小幅回落时不记。
func nextReading(r weekReading, now int64, last readingLine) (line readingLine, ok bool) {
	if r.resetsAt < last.ResetsAt-readingResetsJitter {
		return line, false
	}
	line = readingLine{TS: now, Window: "seven_day", Used: r.used, ResetsAt: r.resetsAt}
	if r.resetsAt-last.ResetsAt > readingResetsJitter {
		return line, true
	}
	line.ResetsAt = last.ResetsAt
	if r.used > last.Used {
		return line, true
	}
	if r.used <= last.Used-readingDropMin {
		from := last.Used
		line.DroppedFrom = &from
		return line, true
	}
	return line, false
}

// recordReading 按 nextReading 把读数追加进 account 的读数文件，并更新旁边的 .state。几个程序同时记时由
// 数据目录里的 rate-limits.lock 排队（flock），读 .state、追加、写 .state 是一整步。
func recordReading(args *Args, account string, r weekReading, now time.Time) (recorded bool, err error) {
	logPath := args.rateLimitLog(account)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return false, err
	}
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(logPath), "rate-limits.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	statePath := strings.TrimSuffix(logPath, ".jsonl") + ".state"
	var last readingLine
	if data, err := os.ReadFile(statePath); err == nil {
		if json.Unmarshal(data, &last) != nil {
			last = readingLine{} // 读不懂的 .state 当作没有上一条：多记一行无害
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	line, ok := nextReading(r, now.Unix(), last)
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

// runRecord 是 agent-usage record：读 stdin、记读数。没有要记的（没带 7 天窗口、读数没变、当前是非账号目标）
// 照常以 0 退出；stdin 不是认得的写法时报错。
func runRecord(args *Args, stdin io.Reader, now time.Time) error {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	r, ok, err := parseReading(data)
	if err != nil || !ok {
		return err
	}
	name, target := args.Account, false
	if name == "" {
		name, target = currentAccount()
	}
	if target {
		return nil
	}
	if !isAccountName(name) {
		return fmt.Errorf("record: 不是账号名: '%s'", name)
	}
	_, err = recordReading(args, name, r, now)
	return err
}
