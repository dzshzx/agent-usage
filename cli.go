// 命令行解析：子命令与选项可任意先后（agent-usage status --text），选项值可写成 --opt v 或 --opt=v；用法错误退出码 2。
package main

import (
	"errors"
	"fmt"
	"github.com/dzshzx/agent-usage/reading"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

var commands = []string{"status", "history", "daily", "prices", "segments", "time", "sync", "export", "import", "peer", "record", "hook", "version"}

const usageLine = "usage: agent-usage [status|history|daily|prices|segments|time|sync [主机…]|export|import|record|hook session-start|version] [选项]\n"

func helpText() string {
	return usageLine + `
Claude Code 各账号（主号 claude 与 claude-switch 管理的其他号）在 7 天限额周期内各模型的 token 用量、按官方价格折算的金额，以及由已用百分比反推的周期总额度。各来源独立报告。默认输出 JSON。

子命令（最多一个，可写在选项前后）：
  status                进行中的周期（默认）
  history               全部周期
  daily                 按日趋势
  prices                价格表与对齐状态
  segments              按 --split 切段对比成本结构、上下文、子代理与 effort
  time                  按 --split 切段看时间花在哪（模型、跑命令、等后台、等用户），直接读会话日志
  sync [主机…]          经 ssh 与各主机互相同步请求与读数：先拉对端、再推本机，只传上次之后的增量；
                        不给主机时同步配置里的全部对端
  export                把库里各账号的请求与读数整份写到 stdout（zstd 压缩的同步格式），另一台用 import 读入
  import                从 stdin 读 export 的输出合并进库
  peer                  sync 在对端执行的一边，经 stdin、stdout 与发起方交换数据
  record                从 stdin 读一份 Claude Code 交出的额度（stream-json 的 rate_limit_event、它的
                        rate_limit_info，或 get_usage 的应答），记下 7 天窗口的已用百分比
  hook session-start    Claude Code 的 SessionStart hook：登记本会话属于哪个账号，不输出
  version               打印版本

选项：
  -h, --help            显示本帮助并退出
  --source ACCOUNT      只看指定账号，可重复；默认全部已知账号
  --account ACCOUNT     record 记到哪个账号（默认按 claude-switch 的切换记录）
  --days DAYS           daily 回看的天数（默认 14）
  --refresh-prices      立即对齐官方价格
  --offline             不联网，只用已采集的价格与读数
  --no-sync             不扫描会话日志，只读数据库
  --max-price-age DAYS  价格超过该天数自动对齐（默认 ` + strconv.FormatFloat(defaultMaxPriceAgeDays, 'g', -1, 64) + `）
  --text                输出给人看的文本而非 JSON
  --by-agent            status/history 另按角色（主会话 main 或子代理角色）、模型、effort、服务档位分组
  --by-session          status/history 另按会话分组（子代理归入主会话），按金额降序，有额度估算时折成额度百分比
  --since TIME          segments、time 的起点（本地时间 YYYY-MM-DD[ HH:MM] 或 RFC 3339；默认 --until 前 7 天）
  --until TIME          segments、time 的终点（默认现在）
  --split TIME          segments、time 的切点，可重复；不给则整段一段
  --remote-command CMD  sync 在对端执行的 agent-usage（默认 ~/.local/bin/agent-usage）
  --config AGENT_USAGE_CONFIG
                        同步配置（默认 ~/.config/agent-usage/config.json）：peers 是对端主机，
                        auto_sync_minutes 是 status、history、daily、segments 顺带同步的最短间隔（0 关闭）
  --claude-home CLAUDE_HOME
  --claude-switch-dir CLAUDE_SWITCH_STATE_DIR
  --data-dir DATA_DIR

退出码：0 成功（可能带 warnings）；1 运行失败；2 参数错误。 数据目录：~/.local/share/agent-usage（数据库、额度读数与会话账号登记）
`
}

func defaultArgs() *Args {
	home, _ := os.UserHomeDir()
	return &Args{
		Command:        "status",
		Days:           14,
		MaxPriceAge:    defaultMaxPriceAgeDays,
		ClaudeHome:     envPath("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude")),
		SwitchStateDir: envPath("CLAUDE_SWITCH_STATE_DIR", filepath.Join(home, ".local/state/claude-switch")),
		DataDir:        reading.Dir(home),
		RemoteCommand:  envPath("AGENT_USAGE_REMOTE_COMMAND", "~/.local/bin/agent-usage"),
		ConfigPath:     envPath("AGENT_USAGE_CONFIG", filepath.Join(home, ".config/agent-usage/config.json")),
	}
}

// parseArgs 解析参数；出错时把用法与错误写到 stderr 并返回 (nil, 2)，--help 写到 stdout 并返回 (nil, 0)。
func parseArgs(argv []string, stdout, stderr io.Writer) (*Args, int) {
	args := defaultArgs()
	fail := func(msg string) (*Args, int) {
		fmt.Fprint(stderr, usageLine)
		fmt.Fprintf(stderr, "agent-usage: error: %s\n", msg)
		return nil, 2
	}
	valued := map[string]*string{
		"--claude-home": &args.ClaudeHome, "--claude-switch-dir": &args.SwitchStateDir,
		"--data-dir": &args.DataDir, "--pricing-url": &args.PricingURL, "--since": &args.Since, "--until": &args.Until,
		"--remote-command": &args.RemoteCommand, "--config": &args.ConfigPath,
		"--account": &args.Account,
	}
	flags := map[string]*bool{
		"--refresh-prices": &args.RefreshPrices, "--offline": &args.Offline, "--no-sync": &args.NoSync,
		"--text": &args.Text, "--by-agent": &args.ByAgent, "--by-session": &args.BySession,
	}
	positional := 0
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "-h" || arg == "--help" {
			fmt.Fprint(stdout, helpText())
			return nil, 0
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if err := setCommand(args, arg, positional); err != nil {
				return fail(err.Error())
			}
			positional++
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if b, ok := flags[name]; ok {
			if hasValue {
				return fail(fmt.Sprintf("argument %s: ignored explicit argument '%s'", name, value))
			}
			*b = true
			continue
		}
		if _, ok := valued[name]; !ok && name != "--source" && name != "--days" && name != "--max-price-age" &&
			name != "--split" {
			return fail(fmt.Sprintf("unrecognized arguments: %s", arg))
		}
		if !hasValue {
			if i+1 >= len(argv) {
				return fail(fmt.Sprintf("argument %s: expected one argument", name))
			}
			i++
			value = argv[i]
		}
		if err := setValueOption(args, valued, name, value); err != nil {
			return fail(err.Error())
		}
	}
	if err := checkOptionCombos(args); err != nil {
		return fail(err.Error())
	}
	return args, 0
}

// setCommand 把位置参数记为子命令：positional 是此前已读到的位置参数个数。第一个须是已知子命令，
// sync 之后的位置参数是对端主机，其余子命令只接受一个位置参数。
func setCommand(args *Args, arg string, positional int) error {
	if positional > 0 {
		if args.Command == "hook" && positional == 1 {
			if arg != "session-start" {
				return fmt.Errorf("argument hook: invalid choice: '%s' (choose from 'session-start')", arg)
			}
			args.HookEvent = arg
			return nil
		}
		if args.Command != "sync" {
			return fmt.Errorf("unrecognized arguments: %s", arg)
		}
		args.Hosts = append(args.Hosts, arg)
		return nil
	}
	if !slices.Contains(commands, arg) {
		return fmt.Errorf("argument command: invalid choice: '%s' (choose from %s)", arg, quotedList(commands))
	}
	args.Command = arg
	return nil
}

// setValueOption 把带值选项 name 的值写进 args；--source、--days、--max-price-age 的值不合法时返回错误。
// valued 是写入单个字符串字段的选项。
func setValueOption(args *Args, valued map[string]*string, name, value string) error {
	switch name {
	case "--source":
		if !isAccountName(value) {
			return fmt.Errorf("argument --source: 不是账号名: '%s'", value)
		}
		args.Sources = append(args.Sources, value)
	case "--split":
		args.Splits = append(args.Splits, value)
	case "--days":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("argument --days: invalid int value: '%s'", value)
		}
		args.Days = n
	case "--max-price-age":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("argument --max-price-age: invalid float value: '%s'", value)
		}
		args.MaxPriceAge = f
	default:
		*valued[name] = value
	}
	return nil
}

// checkOptionCombos 检查选项之间的搭配；segments、time 子命令时由 --since、--until、--split 算出各段边界写进 args.Bounds。
func checkOptionCombos(args *Args) error {
	if args.Command == "hook" && args.HookEvent == "" {
		return errors.New("argument hook: 要给出事件名（session-start）")
	}
	if args.Account != "" && (args.Command != "record" || !isAccountName(args.Account)) {
		return fmt.Errorf("argument --account: 只用于 record，且要是账号名: '%s'", args.Account)
	}
	if args.PricingURL != "" && len(args.Sources) != 1 {
		return errors.New("--pricing-url 需要配合单个 --source")
	}
	if args.Command == "segments" || args.Command == "time" {
		bounds, err := segmentBounds(args.Since, args.Until, args.Splits, time.Now())
		if err != nil {
			return err
		}
		args.Bounds = bounds
	} else if args.Since != "" || args.Until != "" || len(args.Splits) > 0 {
		return errors.New("--since、--until、--split 只用于 segments 与 time")
	}
	if slices.Contains([]string{"export", "import", "peer"}, args.Command) && (args.Text || len(args.Sources) > 0) {
		return errors.New("export、import、peer 不接受 --text 与 --source")
	}
	return nil
}

func quotedList(items []string) string {
	q := make([]string, 0, len(items))
	for _, s := range items {
		q = append(q, "'"+s+"'")
	}
	return strings.Join(q, ", ")
}
