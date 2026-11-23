// 多台机器之间同步用量时传的数据：版本向量、消息格式与导出导入，以及 sync 两端各自的一边（与对端的编排在 sync.go）。
//
// 每行（请求或读数）只由它的出处机器产生和修改，所以每个出处就是一条序号单调递增的日志：本机入库的行 origin 为空、
// 带本机序号 seq（请求与读数共用一个序号空间，请求补全时换新序号）；同步来的行保留出处的库编号与出处内序号。
// 版本向量是「出处 → 已见过的最大序号」：本机自己那一项是本机最大序号，其余记在 origins 表里，因去重没写入的行也算见过。
// 发给对端的只有对端向量之后的行，所以两边已有同样数据时什么都不传；删库重建的一端向量为空，自然拿到全量；
// 第三台机器的行经中间一台照样传过去，同一行不会来回传。
//
// 消息：一串帧，每帧是 4 字节大端长度加一段 zstd 压缩的 JSON Lines，长度 0 的帧表示消息结束；没收到结束帧就断开的消息
// 视为不完整，整份不入库。各行：
//
//	{"t":"hello",…}  首行：格式、版本、库编号、claude-switch 记下的账号编号、版本向量
//	["s", 文本]      字符串表的下一项（编号从 0 起）。来源、文件、模型、档位、角色、effort、出处只在第一次出现时写全文
//	["e", 来源, 消息 id, 时间差, 文件, 模型, 档位, 新输入, 读缓存, 5 分钟写缓存, 1 小时写缓存, 输出, 推理, 角色, effort, 出处, 序号]
//	["r", 来源, ts, 已用百分比, 重置时刻, 回落前的百分比, 出处, 序号]
//
// 字符串字段是字符串表编号，可空的字段（模型、角色、effort、回落前的百分比）空值写 null。时间差是这一行与上一行请求时刻之差
// （毫秒），序号写与上一行（请求、读数不分）之差；新输入是输入减去读缓存与写缓存，收方加回去。导入时请求按消息 id 去重
// （与本机入库同一规则，各账号之间只计一次），金额按收方自己的价格表重算；读数按值去重。
// 两边以账号名对应；两边的 claude-switch 都记了同一个名字、账号编号却不同时，整份拒绝导入。
// sync 的最后一条消息是对端回的导入结果：只有一行 JSON（importStats）。
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	syncFormat  = "agent-usage-sync"
	syncVersion = 4
	importBatch = 1000    // 导入时一条语句写入的请求数
	frameSize   = 1 << 20 // 每帧压缩前的字节数：收方收完一帧就能开始入库
)

type hello struct {
	T        string            `json:"t"`
	Format   string            `json:"format"`
	Version  int               `json:"version"`
	Instance string            `json:"instance"`
	Accounts map[string]string `json:"accounts"`
	Vector   map[string]int64  `json:"vector"`
}

// importStats 是一次导入的结果；sync 的输出里每个方向一份。
type importStats struct {
	From          string `json:"from"`         // 发送方的库编号
	Received      int    `json:"received"`     // 收到的请求与读数行数（含已有、没再写的）
	EventsAdded   int    `json:"events_added"` // 新增或补全的请求；已有的、已记在别的账号下的不算
	ReadingsAdded int    `json:"readings_added"`
}

// readJSONFile 把 path 的 JSON 解析进 out；文件不存在时 out 不动、不算错。
func readJSONFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err == nil {
		err = json.Unmarshal(data, out)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// writeJSONLine 写一行 JSON：消息的 hello 行、对端回的导入结果。
func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err == nil {
		_, err = w.Write(append(b, '\n'))
	}
	return err
}

// switchAccounts 读 claude-switch 的 accounts.json（账号名 → 账号编号）；没有这个文件时为空。
func switchAccounts(args *Args) (map[string]string, error) {
	out := map[string]string{}
	return out, readJSONFile(filepath.Join(args.SwitchStateDir, "accounts.json"), &out)
}

// instanceID 是本库的编号：首次用到时随机生成，删库重建后是新编号。
func (d *DB) instanceID() (string, error) {
	v, err := d.meta("instance")
	if err != nil {
		return "", err
	}
	if v != nil {
		return *v, nil
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// 不覆盖：两个进程同时给新库编号时，以先写进去的为准
	if _, err := d.Exec("INSERT OR IGNORE INTO meta (key, value) VALUES ('instance', ?)", hex.EncodeToString(b)); err != nil {
		return "", err
	}
	return d.instanceID()
}

// vector 是本库的版本向量：本机最大序号与各外来出处见过的最大序号。
func vector(db *DB) (string, map[string]int64, error) {
	instance, err := db.instanceID()
	if err != nil {
		return "", nil, err
	}
	own, err := db.ownSeq()
	if err != nil {
		return "", nil, err
	}
	vec := map[string]int64{instance: own}
	rows, err := db.Query("SELECT origin, seq FROM origins")
	if err != nil {
		return "", nil, err
	}
	for _, r := range rows {
		vec[r.Str("origin")] = r.Int("seq")
	}
	return instance, vec, nil
}

// syncLocal：本机会话日志与各账号的读数先入库，发出去的才是最新的用量。一次运行只做一次。
func (ctx *Context) syncLocal() error {
	if ctx.localSynced {
		return nil
	}
	if _, err := primary.Sync(ctx); err != nil {
		return err
	}
	for _, name := range sourceNames() {
		if err := sources[name].CollectReadings(ctx); err != nil {
			return err
		}
	}
	ctx.localSynced = true
	return nil
}

// export 把整份写到 w（对端向量为空），另一台用 import 读入。
func runExport(args *Args, w io.Writer) error {
	db, ctx, err := openContext(args, !args.NoSync)
	if err != nil {
		return err
	}
	defer db.Close()
	return writeMessage(ctx, newFrameWriter(w), map[string]int64{}, true)
}

func runImport(args *Args, r io.Reader) (*importStats, error) {
	db, ctx, err := openContext(args, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	stats, _, err := readMessage(ctx, newFrameReader(r))
	return stats, err
}

// runPeer 是 sync 在对端执行的一边：收发起方的向量，发它缺的行，再收它发来的、本机缺的行，最后回导入结果。
// 两段是先后的，同一时刻只有一个方向在传、只读或只写库，全程流式：收方收完一帧就入库。
func runPeer(args *Args, r io.Reader, w io.Writer) error {
	db, ctx, err := openContext(args, !args.NoSync)
	if err != nil {
		return err
	}
	defer db.Close()
	return servePeer(ctx, r, w)
}

// servePeer 是 peer 的本体，与发起方的 exchange 一问一答。
func servePeer(ctx *Context, r io.Reader, w io.Writer) error {
	fr, fw := newFrameReader(r), newFrameWriter(w)
	_, theirs, err := readMessage(ctx, fr)
	if err != nil {
		return err
	}
	if err := writeMessage(ctx, fw, theirs, true); err != nil {
		return err
	}
	stats, _, err := readMessage(ctx, fr)
	if err != nil {
		return err
	}
	if err := writeJSONLine(fw, stats); err != nil {
		return err
	}
	return fw.end()
}

// exchange 是 sync 发起方的一边：先报本机的向量，收对端发来的、本机缺的行，再发对端缺的行，最后收对端的导入结果。
// 出错时已经拉回的那份照样返回。
func exchange(ctx *Context, r io.Reader, w io.Writer) (pulled, pushed *importStats, err error) {
	fr, fw := newFrameReader(r), newFrameWriter(w)
	if err := writeMessage(ctx, fw, nil, false); err != nil {
		return nil, nil, err
	}
	pulled, theirs, err := readMessage(ctx, fr)
	if err != nil {
		return nil, nil, err
	}
	if err := writeMessage(ctx, fw, theirs, true); err != nil {
		return pulled, nil, err
	}
	// 读到结束帧为止：对端写完结束帧才退出，提前断开它会写失败
	reply, err := io.ReadAll(fr.message())
	pushed = &importStats{}
	if err != nil || json.Unmarshal(reply, pushed) != nil {
		return pulled, nil, errors.New("对端没有回导入结果")
	}
	return pulled, pushed, nil
}

// writeMessage 写一条消息：hello 与对端向量之后的行；rows 为假时只写 hello（发起方先报向量）。
func writeMessage(ctx *Context, fw *frameWriter, theirs map[string]int64, rows bool) error {
	instance, mine, err := vector(ctx.DB)
	if err != nil {
		return err
	}
	accounts, err := switchAccounts(ctx.Args)
	if err != nil {
		return err
	}
	h := hello{T: "hello", Format: syncFormat, Version: syncVersion, Instance: instance, Accounts: accounts, Vector: mine}
	if err := writeJSONLine(fw, h); err != nil {
		return err
	}
	if rows {
		x := &rowWriter{w: fw, strs: map[string]int{}, accounts: map[string]bool{}}
		for origin := range mine {
			var o *string // 本机的行 origin 为空
			if origin != instance {
				o = &origin
			}
			if err := x.events(ctx.DB, o, origin, theirs[origin]); err != nil {
				return err
			}
			if err := x.readings(ctx.DB, o, origin, theirs[origin]); err != nil {
				return err
			}
		}
	}
	return fw.end()
}

// rowWriter 拼数组行并维护字符串表；时间差相对上一行请求，序号差相对上一行。
type rowWriter struct {
	w        *frameWriter
	strs     map[string]int
	accounts map[string]bool // 来源名 → 是不是账号名：只发账号的行，每个名字只判断一次
	line     []byte
	prevMs   int64
	prevSeq  int64
}

func (x *rowWriter) flushLine() error {
	_, err := x.w.Write(x.line)
	return err
}

func (x *rowWriter) account(name string) bool {
	ok, seen := x.accounts[name]
	if !seen {
		ok = isAccountName(name)
		x.accounts[name] = ok
	}
	return ok
}

// ref 返回字符串在表里的编号，第一次出现时先写一行 ["s", 文本]；nil 为 -1（写成 null）。
func (x *rowWriter) ref(s *string) (int, error) {
	if s == nil {
		return -1, nil
	}
	if i, ok := x.strs[*s]; ok {
		return i, nil
	}
	line := appendJSONString([]byte(`["s",`), *s)
	if _, err := x.w.Write(append(line, "]\n"...)); err != nil {
		return 0, err
	}
	x.strs[*s] = len(x.strs)
	return len(x.strs) - 1, nil
}

func appendRef(b []byte, i int) []byte {
	if i < 0 {
		return append(b, "null"...)
	}
	return strconv.AppendInt(b, int64(i), 10)
}

// appendJSONString 写一个 JSON 字符串；可打印 ASCII 且不含引号、反斜杠时直接加引号，否则交给 encoding/json。
func appendJSONString(b []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f || c == '"' || c == '\\' {
			enc, _ := json.Marshal(s)
			return append(b, enc...)
		}
	}
	return append(append(append(b, '"'), s...), '"')
}

// events 写出出处 o（nil 为本机）序号大于 from 的请求。
func (x *rowWriter) events(db *DB, o *string, origin string, from int64) error {
	const q = "SELECT source, ukey, ts, file, model, tier, input_tokens, cached_input_tokens, cache_write_5m_tokens, " +
		"cache_write_1h_tokens, output_tokens, reasoning_tokens, agent, effort, seq FROM events " +
		"WHERE origin IS ? AND seq > ? AND limit_id = ? ORDER BY seq"
	originRef, err := x.ref(&origin)
	if err != nil {
		return err
	}
	refs := make([]int, 6)
	return db.Each(q, []any{nullableStr(o), from, weekLimitID}, func(scan func(...any) error) error {
		var ev Event
		if err := scan(&ev.Source, &ev.Ukey, &ev.Ts, &ev.File, &ev.Model, &ev.Tier, &ev.InputTokens, &ev.CachedInputTokens,
			&ev.CacheWrite5mTokens, &ev.CacheWrite1hTokens, &ev.OutputTokens, &ev.ReasoningTokens, &ev.Agent, &ev.Effort, &ev.Seq); err != nil {
			return err
		}
		if !x.account(ev.Source) {
			return nil
		}
		for i, s := range []*string{&ev.Source, &ev.File, ev.Model, &ev.Tier, ev.Agent, ev.Effort} {
			if refs[i], err = x.ref(s); err != nil {
				return err
			}
		}
		ms := int64(math.Round(ev.Ts * 1000))
		b := appendRef(append(x.line[:0], `["e",`...), refs[0])
		b = appendJSONString(append(b, ','), ev.Ukey)
		b = strconv.AppendInt(append(b, ','), ms-x.prevMs, 10)
		for _, r := range refs[1:4] {
			b = appendRef(append(b, ','), r)
		}
		for _, n := range []int64{ev.InputTokens - ev.CachedInputTokens - ev.CacheWrite5mTokens - ev.CacheWrite1hTokens,
			ev.CachedInputTokens, ev.CacheWrite5mTokens, ev.CacheWrite1hTokens, ev.OutputTokens, ev.ReasoningTokens} {
			b = strconv.AppendInt(append(b, ','), n, 10)
		}
		b = appendRef(append(b, ','), refs[4])
		b = appendRef(append(b, ','), refs[5])
		b = appendRef(append(b, ','), originRef)
		b = strconv.AppendInt(append(b, ','), ev.Seq-x.prevSeq, 10)
		x.line, x.prevMs, x.prevSeq = append(b, "]\n"...), ms, ev.Seq
		return x.flushLine()
	})
}

// readings 写出出处 o（nil 为本机）序号大于 from 的读数。
func (x *rowWriter) readings(db *DB, o *string, origin string, from int64) error {
	const q = "SELECT source, ts, used_percent, resets_at, dropped_from, seq FROM readings " +
		"WHERE origin IS ? AND seq > ? AND limit_id = ? ORDER BY seq"
	originRef, err := x.ref(&origin)
	if err != nil {
		return err
	}
	return db.Each(q, []any{nullableStr(o), from, weekLimitID}, func(scan func(...any) error) error {
		var source string
		var ts, used float64
		var resets, seq int64
		var dropped *float64
		if err := scan(&source, &ts, &used, &resets, &dropped, &seq); err != nil {
			return err
		}
		if !x.account(source) {
			return nil
		}
		src, err := x.ref(&source)
		if err != nil {
			return err
		}
		b := appendRef(append(x.line[:0], `["r",`...), src)
		b = strconv.AppendFloat(append(b, ','), ts, 'f', -1, 64)
		b = strconv.AppendFloat(append(b, ','), used, 'f', -1, 64)
		b = strconv.AppendInt(append(b, ','), resets, 10)
		if b = append(b, ','); dropped == nil {
			b = append(b, "null"...)
		} else {
			b = strconv.AppendFloat(b, *dropped, 'f', -1, 64)
		}
		b = appendRef(append(b, ','), originRef)
		b = strconv.AppendInt(append(b, ','), seq-x.prevSeq, 10)
		x.line, x.prevSeq = append(b, "]\n"...), seq
		return x.flushLine()
	})
}

// readMessage 读一条消息合并进库，返回导入结果与发送方的版本向量。整条消息在一个事务里，出错或不完整时什么都不写。
func readMessage(ctx *Context, fr *frameReader) (*importStats, map[string]int64, error) {
	br := bufio.NewReaderSize(fr.message(), 1<<16)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, nil, errors.New("收到的内容不完整（连 hello 行都没有）")
	}
	var h hello
	if json.Unmarshal(line, &h) != nil || h.T != "hello" || h.Format != syncFormat {
		return nil, nil, errors.New("收到的不是 agent-usage 的同步数据")
	}
	if h.Version != syncVersion {
		return nil, nil, fmt.Errorf("同步格式版本 %d，本机只认 %d：两边的 agent-usage 版本不同，先更新到同一版", h.Version, syncVersion)
	}
	own, err := ctx.DB.instanceID()
	if err != nil {
		return nil, nil, err
	}
	// 编号相同时两边会把对方的行都当成自己的，同步照样成功却什么也没合并
	if h.Instance == own {
		return nil, nil, fmt.Errorf("两边的库编号相同（%s，库是从另一台拷贝来的），没有导入；在其中一台上执行 "+
			`sqlite3 %s "DELETE FROM meta WHERE key='instance'" 换一个编号后重试`, own, filepath.Join(ctx.Args.DataDir, "usage.db"))
	}
	if err := checkAccounts(ctx.Args, h.Accounts); err != nil {
		return nil, nil, err
	}
	in := &importer{ctx: ctx, own: own, existing: -1, seen: map[string]int64{}, accounts: map[string]bool{},
		stats: &importStats{From: h.Instance}}
	if err := ctx.DB.Tx(func() error { return in.lines(br) }); err != nil {
		return nil, nil, err
	}
	return in.stats, h.Vector, nil
}

// checkAccounts：两边 claude-switch 都记了的账号名，账号编号须相同，否则按名合并会把两个号的用量混在一起。
func checkAccounts(args *Args, theirs map[string]string) error {
	ours, err := switchAccounts(args)
	if err != nil {
		return err
	}
	var conflicts []string
	for name, id := range theirs {
		if mine, ok := ours[name]; ok && mine != id {
			conflicts = append(conflicts, name)
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	slices.Sort(conflicts)
	return fmt.Errorf("账号 %s 在两台机器上是不同的 claude.ai 账号（claude-switch 记下的账号编号不同），没有导入；"+
		"先在一边用 claude-switch 改成一致的账号名", strings.Join(conflicts, "、"))
}

type importer struct {
	ctx      *Context
	own      string // 本库编号
	existing int64  // 导入前库里的请求数；第一次写请求时才数，-1 为还没数
	strs     []string
	prevMs   int64
	prevSeq  int64
	accounts map[string]bool // 已核对过的来源名
	events   int             // 已收的请求数
	batch    []*Event
	seen     map[string]int64 // 各出处这条消息里出现过的最大序号，含没写入的
	stats    *importStats
	// 批量写入：bulk 非空时 events 按消息 id 的唯一索引已删，里面是库里已有的消息 id 的哈希；later 是撞上它们、
	// 等索引重建后再合并的行，条数不超过库里原有的行数（批量写入时它少于这条消息的行数）
	bulk  map[uint64]struct{}
	later []*Event
}

// lines 逐行处理 hello 之后的内容，读到结束帧为止。
func (in *importer) lines(br *bufio.Reader) error {
	var cols [][]byte
	for n := 2; ; n++ {
		line, err := br.ReadSlice('\n') // 只在下一次读之前有效：row 里用到的文本都已复制
		if errors.Is(err, bufio.ErrBufferFull) {
			return fmt.Errorf("第 %d 行太长", n)
		}
		if err != nil && err != io.EOF {
			return fmt.Errorf("收到的内容不完整（%v），整份没有写入", err)
		}
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var rowErr error
			if cols, rowErr = splitArray(line, cols[:0]); rowErr == nil {
				rowErr = in.row(cols)
			}
			if rowErr != nil {
				return fmt.Errorf("第 %d 行: %w", n, rowErr)
			}
		}
		if err == io.EOF {
			return in.finish()
		}
	}
}

// row 处理一个数组行；类型、列数或列的类型不对时报错。
func (in *importer) row(cols [][]byte) error {
	c := columns{cols: cols, strs: in.strs}
	switch kind := c.text(0); {
	case kind == "s" && len(cols) == 2:
		in.strs = append(in.strs, c.text(1))
		return c.err
	case kind == "e" && len(cols) == 17:
		in.stats.Received++
		in.prevMs += c.int(3)
		in.prevSeq += c.int(16)
		ev := &Event{
			Source: c.strOr(1), Ukey: c.text(2), Ts: float64(in.prevMs) / 1000, File: c.strOr(4), Model: c.str(5), Tier: c.strOr(6),
			CachedInputTokens: c.int(8), CacheWrite5mTokens: c.int(9), CacheWrite1hTokens: c.int(10),
			OutputTokens: c.int(11), ReasoningTokens: c.int(12), LimitID: strp(weekLimitID),
			Agent: c.str(13), Effort: c.str(14), Origin: c.ref(15), Seq: in.prevSeq,
		}
		ev.InputTokens = c.int(7) + ev.CachedInputTokens + ev.CacheWrite5mTokens + ev.CacheWrite1hTokens
		if c.err != nil {
			return c.err
		}
		return in.event(ev)
	case kind == "r" && len(cols) == 8:
		in.stats.Received++
		in.prevSeq += c.int(7)
		source, origin := c.strOr(1), c.ref(6)
		ts, used, resets := c.num(2), c.num(3), c.int(4)
		var dropped *float64
		if !c.null(5) {
			dropped = floatp(c.num(5))
		}
		if c.err != nil {
			return c.err
		}
		return in.reading(source, ts, used, resets, dropped, origin, in.prevSeq)
	default:
		return fmt.Errorf("不认识的行（类型 %q，%d 列）", kind, len(cols))
	}
}

// accept 核对来源名、记下出处见过的序号，返回这一行入库时的出处。出处是本库的行（本库从备份恢复后，对端发回
// 本库已经没有的、本机写过的行）按本机的行收下：出处为空，入库时现发序号。
func (in *importer) accept(source string, origin *string, seq int64) (*string, error) {
	if !in.accounts[source] {
		if !isAccountName(source) {
			return nil, fmt.Errorf("来源 %q 不是账号名", source)
		}
		in.accounts[source] = true
		accountSource(source)
	}
	if *origin == in.own {
		return nil, nil
	}
	in.seen[*origin] = max(in.seen[*origin], seq)
	return origin, nil
}

func (in *importer) event(ev *Event) error {
	origin, err := in.accept(ev.Source, ev.Origin, ev.Seq)
	if err != nil {
		return err
	}
	if ev.Ukey == "" {
		return errors.New("消息 id 是空的")
	}
	ev.Origin = origin
	in.events++
	if in.batch = append(in.batch, ev); len(in.batch) >= importBatch {
		return in.flush()
	}
	return nil
}

func (in *importer) reading(source string, ts, used float64, resets int64, dropped *float64, origin *string, seq int64) error {
	origin, err := in.accept(source, origin, seq)
	if err != nil {
		return err
	}
	added, err := in.ctx.addReading(source, weekLimitID, weekMinutes, ts, used, resets, dropped, origin, seq)
	if added {
		in.stats.ReadingsAdded++
	}
	return err
}

// flush 写入攒下的一批请求，按 eventUpsert 与库里已有的合并。
//
// 收的请求比库里原有的还多时改成批量写入：逐行维护按消息 id 等几个索引（随机插入）在手机上要 20 秒以上，
// 删掉它们直接追加、最后一次排序重建只要一半；「比原有的多」保证重建的代价与逐行维护相当或更小，所以从第一批起就可以切换。
// 没有唯一索引时合并不了，已有的消息 id 记成哈希放在内存里，撞上的行（含哈希碰撞）留到索引重建后照常合并，
// 结果与逐批合并相同。
func (in *importer) flush() error {
	batch := in.batch
	in.batch = nil
	if len(batch) == 0 {
		return nil
	}
	if in.bulk == nil {
		if in.existing < 0 {
			n, err := in.ctx.DB.Scalar("SELECT COUNT(*) FROM events")
			if err != nil {
				return err
			}
			in.existing = toInt(n)
		}
		if int64(in.events) > in.existing {
			if err := in.startBulk(); err != nil {
				return err
			}
		}
	}
	if in.bulk == nil {
		return in.write(batch, true)
	}
	fresh := batch[:0]
	for _, ev := range batch {
		h := maphash.String(bulkSeed, ev.Ukey)
		if _, dup := in.bulk[h]; dup {
			in.later = append(in.later, ev)
			continue
		}
		in.bulk[h] = struct{}{}
		fresh = append(fresh, ev)
	}
	return in.write(fresh, false)
}

var bulkSeed = maphash.MakeSeed()

func (in *importer) write(evs []*Event, merge bool) error {
	n, err := in.ctx.writeEvents(evs, merge)
	in.stats.EventsAdded += n
	return err
}

func (in *importer) startBulk() error {
	if err := dropBulkIndexes(in.ctx.DB); err != nil {
		return err
	}
	in.bulk = map[uint64]struct{}{}
	return in.ctx.DB.Each("SELECT ukey FROM events", nil, func(scan func(...any) error) error {
		var ukey string
		err := scan(&ukey)
		in.bulk[maphash.String(bulkSeed, ukey)] = struct{}{}
		return err
	})
}

// endBulk 重建索引，再合并批量写入时留下的行。
func (in *importer) endBulk() error {
	if err := createEventIndexes(in.ctx.DB); err != nil {
		return err
	}
	later := in.later
	for len(later) > 0 {
		n := min(len(later), importBatch)
		if err := in.write(later[:n], true); err != nil {
			return err
		}
		later = later[n:]
	}
	return nil
}

// finish 写完最后一批（批量写入时重建索引），并把各出处见过的序号记进 origins。
func (in *importer) finish() error {
	if err := in.flush(); err != nil {
		return err
	}
	if in.bulk != nil {
		if err := in.endBulk(); err != nil {
			return err
		}
	}
	for origin, seq := range in.seen {
		if _, err := in.ctx.DB.Exec("INSERT INTO origins (origin, seq) VALUES (?, ?) "+
			"ON CONFLICT (origin) DO UPDATE SET seq=MAX(seq, excluded.seq)", origin, seq); err != nil {
			return err
		}
	}
	return nil
}

// splitArray 拆开一个只含数、null 与字符串的 JSON 数组行，把各元素的原文（字符串带引号）追加到 out 后返回。
// 同步格式只有这几种值，专门拆比 encoding/json 解成 []any 快得多。
func splitArray(line []byte, out [][]byte) ([][]byte, error) {
	if len(line) < 2 || line[0] != '[' || line[len(line)-1] != ']' {
		return nil, errors.New("不是数组行")
	}
	body := line[1 : len(line)-1]
	for i := 0; i < len(body); {
		start := i
		if body[i] == '"' {
			for i++; i < len(body) && body[i] != '"'; i++ {
				if body[i] == '\\' {
					i++
				}
			}
			if i >= len(body) {
				return nil, errors.New("字符串没有结束")
			}
			i++
		} else {
			for i < len(body) && body[i] != ',' {
				i++
			}
		}
		out = append(out, body[start:i])
		if i < len(body) {
			if body[i] != ',' {
				return nil, errors.New("元素之间缺逗号")
			}
			i++
		}
	}
	return out, nil
}

// columns 按位置取数组行的值，第一个类型错误记在 err 里。
type columns struct {
	cols [][]byte
	strs []string
	err  error
}

func (c *columns) fail(i int, want string) {
	if c.err == nil {
		c.err = fmt.Errorf("第 %d 列应是%s，实际是 %s", i+1, want, c.cols[i])
	}
}

func (c *columns) null(i int) bool { return string(c.cols[i]) == "null" }

func (c *columns) text(i int) string {
	raw := c.cols[i]
	if len(raw) < 2 || raw[0] != '"' {
		c.fail(i, "字符串")
		return ""
	}
	if !bytes.ContainsRune(raw, '\\') {
		return string(raw[1 : len(raw)-1])
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		c.fail(i, "字符串")
	}
	return s
}

func (c *columns) num(i int) float64 {
	f, err := strconv.ParseFloat(string(c.cols[i]), 64)
	if err != nil {
		c.fail(i, "数")
	}
	return f
}

func (c *columns) int(i int) int64 {
	n, err := strconv.ParseInt(string(c.cols[i]), 10, 64)
	if err != nil {
		c.fail(i, "整数")
	}
	return n
}

// str 取字符串表编号对应的文本；null 为 nil。
func (c *columns) str(i int) *string {
	if c.null(i) {
		return nil
	}
	n, err := strconv.Atoi(string(c.cols[i]))
	if err != nil || n < 0 || n >= len(c.strs) {
		c.fail(i, "字符串表编号")
		return nil
	}
	return &c.strs[n]
}

// ref 同 str，但这一列不能为空：为空时记错误，返回 nil。
func (c *columns) ref(i int) *string {
	s := c.str(i)
	if s == nil {
		c.fail(i, "字符串表编号")
	}
	return s
}

func (c *columns) strOr(i int) string {
	if s := c.ref(i); s != nil {
		return *s
	}
	return ""
}

// frameWriter 把写入的字节攒成帧：攒够 frameSize 压一帧发出；end 发完剩下的并写长度 0 的结束帧。
type frameWriter struct {
	w   io.Writer
	buf []byte // 还没压缩的内容
	out []byte // 长度加压缩后的帧，各帧复用
	enc *zstd.Encoder
}

func newFrameWriter(w io.Writer) *frameWriter {
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	return &frameWriter{w: w, enc: enc}
}

func (f *frameWriter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	if len(f.buf) >= frameSize {
		return len(p), f.frame()
	}
	return len(p), nil
}

func (f *frameWriter) frame() error {
	if len(f.buf) == 0 {
		return nil
	}
	f.out = f.enc.EncodeAll(f.buf, append(f.out[:0], 0, 0, 0, 0))
	binary.BigEndian.PutUint32(f.out, uint32(len(f.out)-4))
	f.buf = f.buf[:0]
	_, err := f.w.Write(f.out)
	return err
}

func (f *frameWriter) end() error {
	if err := f.frame(); err != nil {
		return err
	}
	_, err := f.w.Write([]byte{0, 0, 0, 0})
	return err
}

// frameReader 读帧；message 返回下一条消息的内容，读到结束帧时是 io.EOF，结束帧之前断开时是 io.ErrUnexpectedEOF。
type frameReader struct {
	r     io.Reader
	dec   *zstd.Decoder
	raw   []byte // 压缩的帧，各帧复用
	plain []byte // 解压后的帧，各帧复用
}

func newFrameReader(r io.Reader) *frameReader {
	dec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	return &frameReader{r: r, dec: dec}
}

func (f *frameReader) message() io.Reader { return &messageReader{f: f} }

type messageReader struct {
	f    *frameReader
	buf  []byte // 当前帧还没读走的部分
	done bool
}

func (m *messageReader) Read(p []byte) (int, error) {
	for len(m.buf) == 0 {
		if m.done {
			return 0, io.EOF
		}
		f := m.f
		var n [4]byte
		if _, err := io.ReadFull(f.r, n[:]); err != nil {
			return 0, io.ErrUnexpectedEOF
		}
		size := int(binary.BigEndian.Uint32(n[:]))
		if size == 0 {
			m.done = true
			continue
		}
		f.raw = slices.Grow(f.raw[:0], size)[:size]
		if _, err := io.ReadFull(f.r, f.raw); err != nil {
			return 0, io.ErrUnexpectedEOF
		}
		var err error
		if f.plain, err = f.dec.DecodeAll(f.raw, f.plain[:0]); err != nil {
			return 0, err
		}
		m.buf = f.plain
	}
	n := copy(p, m.buf)
	m.buf = m.buf[n:]
	return n, nil
}
