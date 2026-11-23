// 多台机器之间同步用量：与配置里的对端互相同步，手动、自动与定时三种触发。数据格式与导出导入在 transfer.go。
//
// 各机器的库只从本机的会话日志入库。同一账号在几台机器上用时，额度读数是账号全局的，本机的金额却只含本机的请求，
// 额度估算就偏低。sync 与对端交换版本向量，各自补齐对方缺的行（见 transfer.go），合并后两边都是全部机器的用量。
// 一个对端只开一次 ssh，对端只启动一次 agent-usage（peer）；进度就在数据里，不按对端另记游标。
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// syncResult 是 sync 的输出：每个对端拉回与推送各一份导入结果。
type syncResult struct {
	Hosts    map[string]*peerSync `json:"hosts"`
	Warnings []string             `json:"warnings"`
	failed   bool                 // 有对端出了连不上以外的错，退出码 1
}

type peerSync struct {
	Pulled *importStats `json:"pulled"`
	Pushed *importStats `json:"pushed"`
	Error  string       `json:"error,omitempty"`
}

// remoteRun 经 ssh 在对端执行 agent-usage 的子命令，session 拿到它的标准输入与标准输出；测试替换它。
var remoteRun = func(args *Args, host string, remoteArgs []string, session func(stdin io.WriteCloser, stdout io.Reader) error) error {
	quoted := []string{args.RemoteCommand}
	for _, a := range remoteArgs {
		quoted = append(quoted, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", host, "--", strings.Join(quoted, " "))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sessionErr := session(stdin, bufio.NewReaderSize(stdout, 1<<16))
	stdin.Close()
	if sessionErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if waitErr == nil || sessionErr != nil && stderr.Len() == 0 {
		return sessionErr
	}
	msg := strings.TrimSpace(stderr.String())
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 255 {
		return &unreachableError{msg: "ssh: " + msg}
	}
	return fmt.Errorf("对端 agent-usage %s 失败（%v）：%s", remoteArgs[0], waitErr, msg)
}

// syncConfig 是同步配置文件（Args.ConfigPath，JSON）。没有这个文件时没有对端，也不自动同步。
type syncConfig struct {
	// Peers 是 sync 不给主机时、以及自动同步时的对端（ssh 主机名）
	Peers []string `json:"peers"`
	// AutoSyncMinutes：status、history、daily、segments 运行时，距某个对端上次同步成功超过这么多分钟才顺带同步它；
	// 0 关闭，不写时 defaultAutoSyncMinutes
	AutoSyncMinutes *float64 `json:"auto_sync_minutes"`
}

const defaultAutoSyncMinutes = 60

func loadSyncConfig(args *Args) (*syncConfig, error) {
	var cfg syncConfig
	return &cfg, readJSONFile(args.ConfigPath, &cfg)
}

// unreachableError：ssh 本身失败（退出码 255：连不上、认证失败），对端的 agent-usage 没有跑起来。
type unreachableError struct{ msg string }

func (e *unreachableError) Error() string { return e.msg }

// syncHosts 依次与各对端同步，一台出错不影响其余各台。出错的记进该对端的 error 与警告；
// 连不上（对端离线）以外的错误另让 sync 以退出码 1 结束。
func syncHosts(ctx *Context, hosts []string, result *syncResult) {
	for _, host := range hosts {
		p, err := syncPeer(ctx, host)
		if err == nil {
			result.Hosts[host] = p
			continue
		}
		if p == nil {
			p = &peerSync{}
		}
		p.Error = err.Error()
		result.Hosts[host] = p
		var unreachable *unreachableError
		if errors.As(err, &unreachable) {
			ctx.warn(fmt.Sprintf("同步 %s：连不上，这次跳过（%s）", host, err))
		} else {
			ctx.warn(fmt.Sprintf("同步 %s 失败：%s", host, err))
			result.failed = true
		}
	}
}

func runSync(args *Args) (*syncResult, error) {
	hosts := args.Hosts
	if len(hosts) == 0 {
		cfg, err := loadSyncConfig(args)
		if err != nil {
			return nil, err
		}
		if hosts = cfg.Peers; len(hosts) == 0 {
			return nil, fmt.Errorf("没有给对端主机，%s 里也没有配置 peers", args.ConfigPath)
		}
	}
	db, ctx, err := openContext(args, false) // 本机入库在 syncPeer 里与 ssh 建连同时进行
	if err != nil {
		return nil, err
	}
	defer db.Close()
	result := &syncResult{Hosts: map[string]*peerSync{}}
	syncHosts(ctx, hosts, result)
	result.Warnings = ctx.warnings()
	return result, nil
}

// autoSync：报表命令顺带与配置里到期的对端同步；出错只记警告，报表照出。
// 到期按上次尝试的时刻算，成败都算：对端离线时每个间隔最多试一次，不让每次查报表都等 ssh 超时。
func autoSync(ctx *Context) error {
	cfg, err := loadSyncConfig(ctx.Args)
	if err != nil {
		ctx.warn(fmt.Sprintf("自动同步：%s", err))
		return nil
	}
	minutes := float64(defaultAutoSyncMinutes)
	if cfg.AutoSyncMinutes != nil {
		minutes = *cfg.AutoSyncMinutes
	}
	if minutes <= 0 {
		return nil
	}
	var due []string
	for _, host := range cfg.Peers {
		last, err := ctx.DB.meta(attemptKey(host))
		if err != nil {
			return err
		}
		if last != nil {
			if t, _ := strconv.ParseFloat(*last, 64); now()-t < minutes*60 {
				continue
			}
		}
		due = append(due, host)
	}
	syncHosts(ctx, due, &syncResult{Hosts: map[string]*peerSync{}})
	return nil
}

// attemptKey 是 meta 表里记与某个对端上次尝试同步的时刻的键。
func attemptKey(host string) string { return "sync_attempt:" + host }

// syncPeer 与一个对端互相同步，一次 ssh（两边各做什么见 transfer.go 的 exchange 与 servePeer）。
// ssh 进程起来后才把本机的用量入库：建连、对端启动与本机入库同时进行。
func syncPeer(ctx *Context, host string) (*peerSync, error) {
	if err := ctx.DB.setMeta(attemptKey(host), strconv.FormatFloat(now(), 'f', -1, 64)); err != nil {
		return nil, err
	}
	result := &peerSync{}
	err := remoteRun(ctx.Args, host, []string{"peer"}, func(stdin io.WriteCloser, stdout io.Reader) error {
		if !ctx.Args.NoSync {
			if err := ctx.syncLocal(); err != nil {
				return err
			}
		}
		var err error
		result.Pulled, result.Pushed, err = exchange(ctx, stdout, stdin)
		return err
	})
	return result, err
}

// renderAny 是 --text 的输出：报表与 sync 结果各有写法。
func renderAny(result any) string {
	if s, ok := result.(*syncResult); ok {
		return renderSync(s)
	}
	return renderText(result.(*Result))
}

func renderSync(s *syncResult) string {
	var lines []string
	for _, host := range slices.Sorted(maps.Keys(s.Hosts)) {
		p := s.Hosts[host]
		if p.Error != "" || p.Pulled == nil || p.Pushed == nil {
			lines = append(lines, fmt.Sprintf("%s：没有同步完（%s）", host, p.Error))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s：拉回请求 %d 条、读数 %d 条；推送请求 %d 条、读数 %d 条",
			host, p.Pulled.EventsAdded, p.Pulled.ReadingsAdded, p.Pushed.EventsAdded, p.Pushed.ReadingsAdded))
	}
	for _, w := range s.Warnings {
		lines = append(lines, "warning: "+w)
	}
	return strings.Join(lines, "\n")
}
