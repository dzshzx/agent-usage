// hook session-start：Claude Code 的 SessionStart hook，记下这个会话属于哪个 claude.ai 账号。
//
// 几个账号用 claude-switch 冷切换时共用同一份 ~/.claude/projects 会话日志，日志里看不出哪段属于哪个号；
// 这份登记让 agent-usage 把 token 分回各账号。每次会话启动（startup、resume、clear、compact）向数据目录的
// session-accounts.jsonl 追加一行 {"ts", "session_id", "account"}，当前是 claude-switch 的非账号目标时另带
// "target": true。没有登记的会话归主号 claude，所以只用一个账号时不装这个 hook 也行。
//
// SessionStart 的 stdout 会注入会话上下文，所以从不输出；出错只写 stderr、照常以 0 退出，不影响会话。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dzshzx/claude-switch/account"
)

// switchTargetsDir 是 claude-switch 放非账号目标设置文件的目录（相对 HOME），每个目标一份 <名字>.json，
// 见 claude-switch 的 README「Settings targets」。
const switchTargetsDir = ".config/claude-switch/targets"

// currentAccount 返回本进程所属的名字（claude-switch 的 account.Name）与它是不是非账号目标。
func currentAccount() (name string, target bool) {
	name = account.Name()
	home, err := os.UserHomeDir()
	if err != nil {
		return name, false
	}
	_, err = os.Stat(filepath.Join(home, switchTargetsDir, name+".json"))
	return name, err == nil
}

// sessionEntry 是登记文件里的一行；ts 是 epoch 秒（带小数）。
type sessionEntry struct {
	TS        float64 `json:"ts"`
	SessionID string  `json:"session_id"`
	Account   string  `json:"account"`
	Target    bool    `json:"target,omitempty"`
}

// runHook 是 agent-usage hook <事件>，现在只有 session-start。
func runHook(args *Args, stdin io.Reader, stderr io.Writer, now time.Time) {
	if err := registerSession(args, stdin, now); err != nil {
		fmt.Fprintf(stderr, "agent-usage hook session-start: %s\n", err)
	}
}

func registerSession(args *Args, stdin io.Reader, now time.Time) error {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	var payload struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("stdin 不是 hook 的 JSON: %w", err)
	}
	if payload.SessionID == "" {
		return fmt.Errorf("hook 输入里没有 session_id")
	}
	name, target := currentAccount()
	line, err := json.Marshal(sessionEntry{TS: float64(now.UnixNano()) / 1e9, SessionID: payload.SessionID, Account: name, Target: target})
	if err != nil {
		return err
	}
	path := args.sessionAccountLog()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// 一行一次 O_APPEND 短写，几个会话同时启动也不会互相截断。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
