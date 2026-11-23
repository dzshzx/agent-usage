// 各模块共用的常量、库结构、时间与金额解析、Markdown 表格与 JSONL 增量读取。
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxPriceAgeDays = 7.0
	// 抓取失败，或遇到价格表里没有的新模型时，两次对齐尝试之间的最短间隔
	priceRetrySeconds = 3600
	// 同一周期的 resets_at 在不同响应里会有 1–2 秒抖动
	resetsJitterSeconds = 120
	// 周期以 7 天窗口为单位；请求与读数都挂在这个限额名上
	weekMinutes = 10080
	weekLimitID = "seven_day"
	// 近期参考值：取最近若干个用得够多的周期的估算中位数
	referenceMinUsedPercent = 30
	referenceCycles         = 8
	// 计价规则版本，记在数据库的 user_version；变更时递增，旧库会整体重算金额
	// 1：Codex 不再按长上下文价计（Codex 来源已删，这一版只剩旧库升级时整体重算一次）
	pricingRulesVersion = 1
	// 主会话在 agent 列里的取值；子代理记日志里的角色名
	mainAgent = "main"
)

// eventsTable 建 events 表。索引另列在 eventIndexes 里，不写进表定义：导入大批数据时要先删后建。
const eventsTable = `
CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY,
    source TEXT NOT NULL,
    ukey TEXT NOT NULL,
    ts REAL NOT NULL,
    file TEXT NOT NULL,
    model TEXT,
    tier TEXT NOT NULL,
    input_tokens INTEGER NOT NULL,
    cached_input_tokens INTEGER NOT NULL,
    cache_write_5m_tokens INTEGER NOT NULL,
    cache_write_1h_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    reasoning_tokens INTEGER NOT NULL,
    limit_id TEXT,
    cycle_resets INTEGER,
    cost REAL,
    price_id INTEGER,
    agent TEXT,
    effort TEXT,
    origin TEXT,
    seq INTEGER
);
`

// eventIndexes 是 events 表的全部索引。一条消息（ukey）在各账号之间只记一次，由唯一索引 events_ukey 保证。
// rebuild 的几个在批量导入时先删、导完一次重建（逐行维护要随机插入）；其余两个逐行维护本来就便宜，导入时保留：
// 缺价的部分索引通常是空的，按出处与序号的索引随导入顺序追加。
var eventIndexes = []struct {
	name, ddl string
	rebuild   bool
}{
	{"events_ukey", "CREATE UNIQUE INDEX IF NOT EXISTS events_ukey ON events (ukey)", true},
	{"events_cycle", "CREATE INDEX IF NOT EXISTS events_cycle ON events (source, limit_id, cycle_resets, ts)", true},
	{"events_ts", "CREATE INDEX IF NOT EXISTS events_ts ON events (source, ts)", true},
	{"events_unpriced", "CREATE INDEX IF NOT EXISTS events_unpriced ON events (source, model) WHERE cost IS NULL", false},
	{"events_origin_seq", "CREATE INDEX IF NOT EXISTS events_origin_seq ON events (origin, seq)", false},
}

const schema = `
CREATE TABLE IF NOT EXISTS files (
    source TEXT NOT NULL,
    name TEXT NOT NULL,
    offset INTEGER NOT NULL,
    state TEXT,
    PRIMARY KEY (source, name)
);
` + eventsTable + `CREATE TABLE IF NOT EXISTS readings (
    id INTEGER PRIMARY KEY,
    source TEXT NOT NULL,
    limit_id TEXT NOT NULL,
    window_minutes INTEGER NOT NULL,
    ts REAL NOT NULL,
    used_percent REAL NOT NULL,
    resets_at INTEGER NOT NULL,
    plan TEXT,
    dropped_from REAL,
    origin TEXT,
    seq INTEGER,
    UNIQUE (source, limit_id, ts, used_percent, resets_at)
);
CREATE INDEX IF NOT EXISTS readings_cycle ON readings (source, limit_id, resets_at, ts);
CREATE INDEX IF NOT EXISTS readings_origin_seq ON readings (origin, seq);
CREATE TABLE IF NOT EXISTS prices (
    id INTEGER PRIMARY KEY,
    source TEXT NOT NULL,
    model TEXT NOT NULL,
    tier TEXT NOT NULL,
    valid_from REAL NOT NULL,
    input REAL NOT NULL,
    cached_input REAL,
    cache_write REAL,
    cache_write_1h REAL,
    output REAL NOT NULL,
    long_input REAL,
    long_cached_input REAL,
    long_cache_write REAL,
    long_output REAL,
    long_threshold INTEGER,
    url TEXT NOT NULL,
    long_cache_write_1h REAL
);
CREATE INDEX IF NOT EXISTS prices_lookup ON prices (source, model, tier, valid_from);
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS origins (
    origin TEXT PRIMARY KEY,
    seq INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS price_checks (
    source TEXT NOT NULL,
    checked_at REAL NOT NULL,
    ok INTEGER NOT NULL,
    models INTEGER,
    changed INTEGER,
    detail TEXT
);
`

// now 取当前时刻（epoch 秒），测试可替换。
var now = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// iso 把 epoch 秒格式化为本地时区、精确到秒的 ISO 8601 字符串；nil 原样返回。
func iso(ts *float64) *string {
	if ts == nil {
		return nil
	}
	return strp(isoOf(*ts))
}

func isoOf(ts float64) string {
	sec, frac := math.Modf(ts)
	return time.Unix(int64(sec), int64(frac*1e9)).Local().Format("2006-01-02T15:04:05-07:00")
}

// parseTs 解析日志里的 ISO 8601 时间戳（Z 或带偏移）为 epoch 秒；不带时区的按本地时间。
func parseTs(value string) (float64, error) {
	t, err := time.Parse(time.RFC3339Nano, strings.Replace(value, "Z", "+00:00", 1))
	if err != nil {
		var naive error
		t, naive = time.ParseInLocation("2006-01-02T15:04:05.999999999", value, time.Local)
		if naive != nil {
			return 0, lineErr("时间戳 %q 不是 ISO 8601 写法", value)
		}
	}
	return float64(t.UnixMicro()) / 1e6, nil
}

var moneyRe = regexp.MustCompile(`\$\s*([0-9][0-9,]*(?:\.[0-9]+)?)`)

func parseMoney(cell string) *float64 {
	cell = strings.TrimSpace(cell)
	if strings.ToLower(cell) == "free" {
		return floatp(0)
	}
	m := moneyRe.FindStringSubmatch(cell)
	if m == nil {
		return nil
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
	if err != nil {
		return nil
	}
	return floatp(v)
}

// mdTable 是 Markdown 文本里的一张表：表前最近的非空文本行、表头（已转小写）与各行单元格，均已去空白。
type mdTable struct {
	context []string
	header  []string
	rows    [][]string
}

// markdownTables 按出现顺序产出各表；分隔行与列数不等的行跳过。
func markdownTables(text string) []mdTable {
	var out []mdTable
	var context []string
	var cur *mdTable
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "|") {
			if line != "" {
				if cur != nil {
					context = nil
				}
				cur = nil
				context = append(context, line)
			}
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if cur == nil {
			header := make([]string, len(cells))
			for i, c := range cells {
				header[i] = strings.ToLower(c)
			}
			out = append(out, mdTable{context: append([]string(nil), context...), header: header})
			cur = &out[len(out)-1]
			continue
		}
		if len(cells) != len(cur.header) || isSeparatorRow(cells) {
			continue
		}
		cur.rows = append(cur.rows, cells)
	}
	return out
}

func isSeparatorRow(cells []string) bool {
	for _, c := range cells {
		if strings.Trim(c, "-: ") != "" {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- 日志读取

// lineError 是单行日志字段缺失或结构异常时的错误；只跳过这一行，同一文件的其他行照常入库。
// 文本写明字段与期望类型，只进 warning 给人看。
type lineError struct{ msg string }

func (e *lineError) Error() string { return e.msg }

func lineErr(format string, args ...any) error { return &lineError{fmt.Sprintf(format, args...)} }
func isLineError(err error) bool               { var le *lineError; return errors.As(err, &le) }

const badLinesShown = 10

type badLine struct {
	start int64
	err   error
}

// readJSONL 从 offset 起把每个完整行交给 handle，它返回该行新增的条数；返回新 offset 与新增总数。
// 末行尚未写完就停，下次再读。某行字段缺失或结构异常时跳过该行、记一条带路径与行号的 warning，
// 进度照常越过它，坏行不会让之后的每次运行都失败。其他错误（数据库等）原样返回。
func readJSONL(ctx *Context, source, path string, offset int64, handle func(line []byte) (int, error)) (int64, int, error) {
	fh, err := os.Open(path)
	if err != nil {
		return offset, 0, err
	}
	defer fh.Close()
	if _, err := fh.Seek(offset, io.SeekStart); err != nil {
		return offset, 0, err
	}
	added := 0
	var bad []badLine
	reader := bufio.NewReaderSize(fh, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break // 末行没有换行符：下次再读
			}
			return offset, added, err
		}
		start := offset
		offset += int64(len(line))
		n, err := handle(line)
		if err != nil {
			if isLineError(err) {
				bad = append(bad, badLine{start, err})
				continue
			}
			return offset, added, err
		}
		added += n
	}
	if len(bad) > 0 {
		ctx.warn(badLinesWarning(source, path, bad))
	}
	return offset, added, nil
}

// badLinesWarning 的 bad 按偏移升序；行号从 1 起，只在出错时数一次换行。
func badLinesWarning(source, path string, bad []badLine) string {
	head, _ := readPrefix(path, bad[len(bad)-1].start)
	var linenos []string
	pos, lineno := int64(0), 1
	for _, b := range bad {
		lineno += bytes.Count(head[pos:b.start], []byte{'\n'})
		pos = b.start
		linenos = append(linenos, strconv.Itoa(lineno))
	}
	shown := strings.Join(linenos[:min(len(linenos), badLinesShown)], ", ")
	if len(linenos) > badLinesShown {
		shown += "…"
	}
	detail := truncateRunes(bad[0].err.Error(), 160)
	return fmt.Sprintf("%s: 跳过 %d 行字段缺失或结构异常的日志 %s 第 %s 行（首个错误 %s）", source, len(bad), path, shown, detail)
}

func readPrefix(path string, n int64) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	buf := make([]byte, n)
	read, err := io.ReadFull(fh, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return buf[:read], err
	}
	return buf[:read], nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func strp(s string) *string     { return &s }
func floatp(f float64) *float64 { return &f }
func intp(i int64) *int64       { return &i }

// zipColumns 把表头与一行单元格配成列名 → 单元格。
func zipColumns(header, cells []string) map[string]string {
	col := make(map[string]string, len(header))
	for i, h := range header {
		if i < len(cells) {
			col[h] = cells[i]
		}
	}
	return col
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// globRecursive 返回 dir 下（不跟随符号链接）文件名匹配 pattern 的全部文件。
func globRecursive(dir, pattern string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if ok, _ := filepath.Match(pattern, d.Name()); ok {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// roundTo 把 x 舍入到 n 位小数：经十进制格式化，按 x 的精确二进制值正确舍入，恰在中点时取偶。
func roundTo(x float64, n int) float64 {
	if math.IsInf(x, 0) || math.IsNaN(x) {
		return x
	}
	v, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	if err != nil {
		return x
	}
	return v
}

// median 是中位数；偶数个时取中间两个的平均。
func median(values []float64) float64 {
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}
