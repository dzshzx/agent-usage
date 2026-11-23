// agent-usage: Claude Code 各账号在 7 天限额周期内的 token 用量、折算金额与周期总额度估算。
//
// 数据流：Claude Code 会话日志 → 增量入库 (SQLite) → 按官方价格折算 → 按限额周期汇总。
// 每个来源（账号）独立报告。默认输出 JSON 到 stdout；警告同时写进 JSON 的 warnings 字段。
// 命令行解析在 cli.go，多台机器之间的同步在 sync.go。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Args 是解析后的命令行参数。
type Args struct {
	Command        string
	Sources        []string
	Days           int
	RefreshPrices  bool
	Offline        bool
	NoSync         bool
	MaxPriceAge    float64
	Text           bool
	ByAgent        bool
	BySession      bool
	ClaudeHome     string
	SwitchStateDir string // claude-switch 的状态目录，同步时核对账号编号
	DataDir        string // 数据库 usage.db、读数 rate-limits[.<账号>].jsonl 与会话账号登记 session-accounts.jsonl 所在目录
	PricingURL     string
	Since          string
	Until          string
	Splits         []string
	Bounds         []float64 // segments、time 的各段边界，parseArgs 算好
	Hosts          []string  // sync 的对端（ssh 主机名）；不给时取配置里的 peers
	RemoteCommand  string    // sync 在对端执行的 agent-usage
	ConfigPath     string    // 同步配置：对端与自动同步间隔，见 sync.go 的 syncConfig
	Account        string    // record 记到哪个账号；空时按 claude-switch 的切换记录
	HookEvent      string    // hook 的事件名（session-start）
}

// rateLimitLog 是账号的 7 天窗口读数文件：主号 rate-limits.jsonl，其余 rate-limits.<账号>.jsonl。
func (a *Args) rateLimitLog(account string) string {
	name := "rate-limits.jsonl"
	if account != primaryAccount {
		name = "rate-limits." + account + ".jsonl"
	}
	return filepath.Join(a.DataDir, name)
}

// sessionAccountLog 是会话账号登记文件。
func (a *Args) sessionAccountLog() string {
	return filepath.Join(a.DataDir, "session-accounts.jsonl")
}

// envPath：环境变量为空串时同未设置，否则空路径会退化成当前目录下的相对路径。
func envPath(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// repriceOnRuleChange：计价规则变更后，已入库的请求按新规则重算一次。新库里还没有请求，重算是空的，直接记上当前版本。
func repriceOnRuleChange(ctx *Context) error {
	db := ctx.DB
	version, err := db.userVersion()
	if err != nil {
		return err
	}
	if version >= pricingRulesVersion {
		return nil
	}
	for _, name := range sourceNames() {
		if err := ctx.RepriceAll(sources[name]); err != nil {
			return err
		}
	}
	return db.setUserVersion(pricingRulesVersion)
}

// alignPrices 补齐价格并重算缺价请求；返回本次是否刷新过价格。
func alignPrices(db *DB, ctx *Context, src Source, refreshed bool) (bool, error) {
	// 上次对齐之后才出现的新模型触发提前对齐；一直查不到价的旧模型不反复重试
	aligned, err := lastAlignedAt(db, src)
	if err != nil {
		return refreshed, err
	}
	since := 0.0
	if aligned != nil {
		since = *aligned
	}
	freshUnknown, err := db.QueryRow(
		"SELECT 1 FROM events WHERE source=? AND cost IS NULL AND model IS NOT NULL AND ts>? LIMIT 1", src.Name(), since)
	if err != nil {
		return refreshed, err
	}
	if freshUnknown != nil && !refreshed {
		if refreshed, err = ctx.ensurePrices(src, true); err != nil {
			return refreshed, err
		}
	}
	unpriced, err := db.QueryRow("SELECT 1 FROM events WHERE source=? AND cost IS NULL LIMIT 1", src.Name())
	if err != nil {
		return refreshed, err
	}
	if unpriced != nil {
		if err := ctx.RepriceMissing(src); err != nil {
			return refreshed, err
		}
	}
	return refreshed, nil
}

// addCycles：status/history：周期报告、额度参考与缺价模型警告写进 block。onlyActive 为真时（status）只给进行中的周期
// 出完整报告（按模型拆分要把整个周期的请求分组排序），别的周期只为参考额度算额度估算，凑够 referenceCycles 个就停。
func addCycles(block *SourceBlock, db *DB, ctx *Context, src Source, onlyActive bool) error {
	args, name := ctx.Args, src.Name()
	cycles, err := listCycles(db, src)
	if err != nil {
		return err
	}
	reports := make([]*CycleReport, 0, len(cycles))
	var picked []float64 // 参考额度用的估算，新的在前
	for _, c := range cycles {
		full := !onlyActive || c.active()
		// 推断周期没有读数，进不了参考
		if !full && (c.Inferred || len(picked) == referenceCycles) {
			continue
		}
		q, err := loadCycleQuota(db, src, c)
		if err != nil {
			return err
		}
		if full {
			r, err := cycleReport(db, ctx, src, c, q, args.ByAgent, args.BySession)
			if err != nil {
				return err
			}
			reports = append(reports, r)
		}
		if est, ok := q.reference(); ok && len(picked) < referenceCycles {
			picked = append(picked, est)
		}
	}
	block.hasQuota = true
	block.QuotaReference = quotaReference(picked)
	if onlyActive && len(reports) == 0 {
		ctx.warn(fmt.Sprintf("%s: 当前没有进行中的周期（本周期内尚无读数）", name))
	}
	block.Cycles = reports
	warnUnpriced(ctx, name, reports)
	return nil
}

// warnUnpriced：周期报告里有在价格表中找不到的模型时记一条警告。
func warnUnpriced(ctx *Context, name string, reports []*CycleReport) {
	seen := map[string]bool{}
	var unpriced []string
	for _, r := range reports {
		for _, m := range r.Models {
			if m.UnpricedRequests == 0 {
				continue
			}
			label := "(未知模型)"
			if m.Model != nil && *m.Model != "" {
				label = *m.Model
			}
			if !seen[label] {
				seen[label] = true
				unpriced = append(unpriced, label)
			}
		}
	}
	if len(unpriced) > 0 {
		sortStrings(unpriced)
		ctx.warn(fmt.Sprintf("%s: 以下模型在价格表中找不到，其请求未计入金额: %s", name, strings.Join(unpriced, ", ")))
	}
}

// sourceBlock：单个来源的报告块：同步日志、对齐价格，再按子命令出报告。
func sourceBlock(db *DB, ctx *Context, src Source) (*SourceBlock, error) {
	args, name := ctx.Args, src.Name()
	block := &SourceBlock{Currency: src.Currency(), Symbol: src.Symbol()}
	refreshed, err := ctx.ensurePrices(src, false)
	if err != nil {
		return nil, err
	}
	if !args.NoSync {
		stats, err := sessionStats(ctx, src)
		if err != nil {
			return nil, err
		}
		block.Sync = &stats
	}
	if refreshed, err = alignPrices(db, ctx, src, refreshed); err != nil {
		return nil, err
	}
	state, err := priceState(db, src)
	if err != nil {
		return nil, err
	}
	state.RefreshedNow = refreshed
	block.Prices = state
	if state.LastAlignedAt == nil {
		ctx.warn(fmt.Sprintf("%s: 尚无已采集价格，金额无法计算；联网后加 --refresh-prices 重跑（不加时距上次尝试满一小时才再抓）", name))
	}
	switch args.Command {
	case "prices":
		if block.PricesTable, err = latestPrices(db, src); err != nil {
			return nil, err
		}
	case "daily":
		if block.Daily, err = dailyReport(db, src, max(args.Days, 1)); err != nil {
			return nil, err
		}
	case "segments":
		book, err := ctx.Book(src, false)
		if err != nil {
			return nil, err
		}
		if block.Segments, err = segmentsReport(db, book, src, args.Bounds); err != nil {
			return nil, err
		}
	default:
		if err := addCycles(block, db, ctx, src, args.Command == "status"); err != nil {
			return nil, err
		}
	}
	return block, nil
}

// run 执行一次命令行调用，返回退出码：0 成功（可能带 warnings）；1 运行失败；2 参数错误。
func run(argv []string, stdout, stderr io.Writer) int {
	args, code := parseArgs(argv, stdout, stderr)
	if args == nil {
		return code
	}
	var result any
	var err error
	switch args.Command {
	case "export":
		err = runExport(args, stdout)
	case "import":
		result, err = runImport(args, os.Stdin)
	case "peer":
		err = runPeer(args, os.Stdin, stdout)
	case "record":
		err = runRecord(args, os.Stdin, time.Now())
	case "hook":
		runHook(args, os.Stdin, stderr, time.Now())
	case "version":
		fmt.Fprintln(stdout, "agent-usage", buildVersion(version))
	case "sync":
		var r *syncResult
		if r, err = runSync(args); err == nil {
			result = r
		}
	default:
		result, err = execute(args)
	}
	if err != nil {
		fmt.Fprintf(stderr, "agent-usage: %s\n", err)
		return 1
	}
	if result == nil {
		return 0
	}
	if args.Text {
		fmt.Fprintln(stdout, renderAny(result))
	} else {
		out, err := marshalIndent(result)
		if err != nil {
			fmt.Fprintf(stderr, "agent-usage: %s\n", err)
			return 1
		}
		fmt.Fprintln(stdout, out)
	}
	if r, ok := result.(*syncResult); ok && r.failed {
		return 1
	}
	return 0
}

// openContext 打开库、重新识别账号（见 discoverAccounts），返回入库上下文；local 为真时先把本机的用量入库
// （会话日志里新出现的账号在入库时登记，之后才列得到）。入库与同步来的请求写入时就按价格表算好金额，
// 所以先对齐价格与计价规则：新库先拉价格（否则几十万行要等第一次出报表时整批补算，手机上要好几秒），
// 规则版本旧的库先按新规则重算。
func openContext(args *Args, local bool) (*DB, *Context, error) {
	db, err := openDB(args.DataDir)
	if err != nil {
		return nil, nil, err
	}
	ctx, err := newContext(db, args)
	if err == nil {
		resetSources()
		err = discoverAccounts(ctx)
	}
	if err == nil {
		err = ensureFirstPrices(ctx)
	}
	if err == nil {
		err = repriceOnRuleChange(ctx)
	}
	if err == nil && local {
		err = ctx.syncLocal()
	}
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, ctx, nil
}

// ensureFirstPrices：库里还没有对齐过价格（新库、删库重建）时先拉价格；之后照常由报表命令按期对齐。
func ensureFirstPrices(ctx *Context) error {
	aligned, err := lastAlignedAt(ctx.DB, primary)
	if err == nil && aligned == nil {
		_, err = ctx.ensurePrices(primary, false)
	}
	return err
}

func execute(args *Args) (*Result, error) {
	if args.Command == "time" {
		return timeResult(args), nil
	}
	db, ctx, err := openContext(args, !args.NoSync)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if !args.NoSync && !args.Offline && args.Command != "prices" {
		if err := autoSync(ctx); err != nil {
			return nil, err
		}
	}
	result := &Result{GeneratedAt: isoOf(now()), Sources: map[string]*SourceBlock{}}
	names := args.Sources
	if len(names) == 0 {
		names = sourceNames()
	}
	for _, name := range names {
		block, err := sourceBlock(db, ctx, accountSource(name))
		if err != nil {
			return nil, err
		}
		result.Sources[name] = block
	}
	result.Warnings = ctx.warnings()
	return result, nil
}

// marshalIndent 编码整份输出：两格缩进，不转义非 ASCII 与 <、>、&。
func marshalIndent(v any) (string, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
