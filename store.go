// 入库：限额周期归并与同步上下文。
package main

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
)

// fileState 是一个会话文件的解析状态，随读取进度存在 files.state 里（JSON）。
type fileState struct {
	Agent *string `json:"agent,omitempty"`
}

type cycleEntry struct {
	resets  int64
	maxUsed float64
}

// CycleIndex 把读数归到限额周期，周期以首次见到的 resets_at 为键。
//
// 两种情况下 resets_at 不同仍属同一周期：响应之间 1–2 秒的抖动；used_percent 还是 0 时
// 窗口尚未锚定，resets_at 随每次请求向后滚动。后者一旦出现非零读数就以当时的值定键。
type CycleIndex struct {
	db    *DB
	known map[[2]string][]*cycleEntry
}

func newCycleIndex(db *DB) (*CycleIndex, error) {
	rows, err := db.Query("SELECT source, limit_id, resets_at, MAX(used_percent) used FROM readings GROUP BY 1, 2, 3")
	if err != nil {
		return nil, err
	}
	idx := &CycleIndex{db: db, known: map[[2]string][]*cycleEntry{}}
	for _, r := range rows {
		k := [2]string{r.Str("source"), r.Str("limit_id")}
		idx.known[k] = append(idx.known[k], &cycleEntry{resets: r.Int("resets_at"), maxUsed: r.Float("used")})
	}
	return idx, nil
}

// Canonical 返回读数所属周期的键（resets_at），必要时把未锚定周期的键改写到新值。
func (c *CycleIndex) Canonical(source, limitID string, window int64, resets int64, used float64) (int64, error) {
	k := [2]string{source, limitID}
	for _, e := range c.known[k] {
		if abs64(e.resets-resets) <= resetsJitterSeconds {
			e.maxUsed = math.Max(e.maxUsed, used)
			return e.resets, nil
		}
	}
	for _, e := range c.known[k] {
		if e.maxUsed == 0 && abs64(e.resets-resets) < window*60 {
			if used > 0 || resets > e.resets {
				for _, q := range []string{
					"UPDATE readings SET resets_at=? WHERE source=? AND limit_id=? AND resets_at=?",
					"UPDATE events SET cycle_resets=? WHERE source=? AND limit_id=? AND cycle_resets=?",
				} {
					if _, err := c.db.Exec(q, resets, source, limitID, e.resets); err != nil {
						return 0, err
					}
				}
				e.resets = resets
			}
			e.maxUsed = used
			return e.resets, nil
		}
	}
	c.known[k] = append(c.known[k], &cycleEntry{resets: resets, maxUsed: used})
	return resets, nil
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

type accountRow struct {
	ts      float64
	account string
}

// Context 是一次运行的同步上下文：数据库、参数、警告与跨来源共享的状态。
type Context struct {
	DB       *DB
	Args     *Args
	Warnings []string
	Cycles   *CycleIndex
	books    map[string]*PriceBook
	// 本次运行已尝试过对齐的价格表来源 → 是否刷新成功：同一次运行里不再抓第二次（见 ensurePrices）
	priceAttempts map[string]bool
	localSynced   bool // 本次运行已把本机的用量入库（见 syncLocal）
	// 主号本轮会话日志扫描的统计与会话账号登记；别的账号复用，不再扫一遍日志
	claudeSync      *claudeSyncStats
	sessionAccounts map[string][]accountRow
	claudeAdded     map[string]int
	claudeDeferred  []*Event
}

type claudeSyncStats struct {
	SyncStats
	added map[string]int
}

func newContext(db *DB, args *Args) (*Context, error) {
	cycles, err := newCycleIndex(db)
	if err != nil {
		return nil, err
	}
	return &Context{DB: db, Args: args, Cycles: cycles, books: map[string]*PriceBook{}, priceAttempts: map[string]bool{},
		claudeAdded: map[string]int{}}, nil
}

func (ctx *Context) warn(msg string) { ctx.Warnings = append(ctx.Warnings, msg) }

// warnings 是输出用的警告列表：没有警告时是空数组而不是 null。
func (ctx *Context) warnings() []string {
	if ctx.Warnings == nil {
		return []string{}
	}
	return ctx.Warnings
}

// Book 按价格表来源缓存：共用价格表的来源拿到同一本。
func (ctx *Context) Book(src Source, reload bool) (*PriceBook, error) {
	owner := priceOwner(src)
	if book, ok := ctx.books[owner.Name()]; ok && !reload {
		return book, nil
	}
	book, err := newPriceBook(ctx.DB, src)
	if err != nil {
		return nil, err
	}
	ctx.books[owner.Name()] = book
	return book, nil
}

// testHookAddEvent 在写入前被调用，测试用它注入失败。
var testHookAddEvent func(ev *Event) error

// AddEvent 写入一次请求；同一请求再次出现时，只在后到的记录 output 计数更完整时覆盖计数与金额，
// 角色与 effort 只补空值，旧库重扫时借此回填。返回是否写入或更新了行。
func (ctx *Context) AddEvent(src Source, ev *Event) (bool, error) {
	if testHookAddEvent != nil {
		if err := testHookAddEvent(ev); err != nil {
			return false, err
		}
	}
	ev.Source = src.Name()
	n, err := ctx.writeEvents([]*Event{ev}, true)
	return n > 0, err
}

// eventFields 是 events 表除 id 以外的全部列，顺序与 eventValues 一致。
var eventFields = []string{"source", "ukey", "ts", "file", "model", "tier", "input_tokens", "cached_input_tokens",
	"cache_write_5m_tokens", "cache_write_1h_tokens", "output_tokens", "reasoning_tokens", "limit_id", "cycle_resets", "cost",
	"price_id", "agent", "effort", "origin", "seq"}

var eventColumns = strings.Join(eventFields, ", ")

// eventUpsert：同一消息再次写入时的合并规则。真改了行（输出更完整、补上角色或 effort）才换序号，同步时这一行再传一遍；
// 一行只由它的出处、记在它的账号下改：本机日志与同步来的同一消息互不覆盖，已记在别的账号下的消息不再记（各账号之间只计一次）。
var eventUpsert = func() string {
	const more = "excluded.output_tokens > events.output_tokens"
	var sets []string
	for _, f := range []string{"output_tokens", "reasoning_tokens", "cost", "price_id"} {
		sets = append(sets, f+"=CASE WHEN "+more+" THEN excluded."+f+" ELSE events."+f+" END")
	}
	return " ON CONFLICT (ukey) DO UPDATE SET " + strings.Join(sets, ", ") +
		", agent=COALESCE(events.agent, excluded.agent), effort=COALESCE(events.effort, excluded.effort), seq=excluded.seq " +
		"WHERE (" + more + " OR (events.agent IS NULL AND excluded.agent IS NOT NULL) " +
		"OR (events.effort IS NULL AND excluded.effort IS NOT NULL)) AND events.origin IS excluded.origin AND events.source = excluded.source"
}()

// eventValues 把一行的值按 eventFields 的顺序追加到 args。重复的文本（来源、文件、模型等）从 boxed 里取同一个值，
// 同步导入几十万行时少做几百万次分配。
func eventValues(args []any, ev *Event, boxed map[string]any) []any {
	text := func(s *string) any {
		if s == nil {
			return nil
		}
		v, ok := boxed[*s]
		if !ok {
			v = *s
			boxed[*s] = v
		}
		return v
	}
	return append(args, text(&ev.Source), ev.Ukey, ev.Ts, text(&ev.File), text(ev.Model), text(&ev.Tier), ev.InputTokens,
		ev.CachedInputTokens, ev.CacheWrite5mTokens, ev.CacheWrite1hTokens, ev.OutputTokens, ev.ReasoningTokens,
		text(ev.LimitID), nullableInt(ev.CycleResets), nullable(ev.Cost), nullableInt(ev.PriceID), text(ev.Agent),
		text(ev.Effort), text(ev.Origin), ev.Seq)
}

// writeEvents 用一条语句写入一批来源已定的请求：按价格表算金额，本机入库的（没有出处）现发序号。返回新增或补全的条数。
// 同步导入几十万条时按批写，比逐条执行快几倍。merge 为假时直接追加、不按 eventUpsert 合并（批量导入删了索引时用）。
func (ctx *Context) writeEvents(evs []*Event, merge bool) (int, error) {
	if len(evs) == 0 {
		return 0, nil
	}
	book, err := ctx.Book(primary, false) // 各账号都用主号的价格表（见 priceOwner），一批里来源不同也是同一本
	if err != nil {
		return 0, err
	}
	args := make([]any, 0, len(evs)*len(eventFields))
	boxed := map[string]any{}
	for _, ev := range evs {
		ev.Cost, ev.PriceID = book.Cost(ev)
		if ev.Origin == nil {
			if ev.Seq, err = ctx.DB.nextSeq(); err != nil {
				return 0, err
			}
		}
		args = eventValues(args, ev, boxed)
	}
	row := "(" + placeholders(len(eventFields)) + ")"
	q := "INSERT INTO events (" + eventColumns + ") VALUES " + strings.TrimSuffix(strings.Repeat(row+", ", len(evs)), ", ")
	if merge {
		q += eventUpsert
	}
	res, err := ctx.DB.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// AddReading 写入一条额度读数，resets 先归并到已知周期。droppedFrom 非空表示这条是
// 同一周期内的回落（官方中途清零或重算档位），值是回落前的百分比。
func (ctx *Context) AddReading(source, limitID string, window int64, ts, used float64, resets int64, droppedFrom *float64) (bool, error) {
	return ctx.addReading(source, limitID, window, ts, used, resets, droppedFrom, nil, 0)
}

// addReading 同 AddReading；origin 与 seq 是从别的机器同步来的读数的出处与出处内序号，本机的（origin 为 nil）现发序号。
func (ctx *Context) addReading(source, limitID string, window int64, ts, used float64, resets int64, droppedFrom *float64, origin *string, seq int64) (bool, error) {
	resets, err := ctx.Cycles.Canonical(source, limitID, window, resets, used)
	if err != nil {
		return false, err
	}
	if origin == nil {
		if seq, err = ctx.DB.nextSeq(); err != nil {
			return false, err
		}
	}
	res, err := ctx.DB.Exec(
		"INSERT OR IGNORE INTO readings (source, limit_id, window_minutes, ts, used_percent, resets_at, dropped_from, origin, seq) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", source, limitID, window, ts, used, resets, nullable(droppedFrom), nullableStr(origin), seq)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ingestFunc 从 offset 起读一个文件，返回新 offset 与新增条数；state 可就地更新。
type ingestFunc func(ctx *Context, path, name string, offset int64, state *fileState) (int64, int, error)

// SyncFiles 增量读取各文件，进度与状态写回 files 表；finish 在全部文件读完、提交之前调用。
// 请求、读数、读取进度与 finish 的写入在同一事务里：中途出错整轮回滚，下次从上次的进度重读。
func (ctx *Context) SyncFiles(src Source, paths []string, nameOf func(string) string, ingest ingestFunc, finish func(*Context) error) (SyncStats, error) {
	var stats SyncStats
	err := ctx.DB.Tx(func() error { return ctx.syncFiles(src, paths, nameOf, ingest, finish, &stats) })
	return stats, err
}

func (ctx *Context) syncFiles(src Source, paths []string, nameOf func(string) string, ingest ingestFunc, finish func(*Context) error, stats *SyncStats) error {
	rows, err := ctx.DB.Query("SELECT * FROM files WHERE source=?", src.Name())
	if err != nil {
		return err
	}
	states := map[string]Row{}
	for _, r := range rows {
		states[r.Str("name")] = r
	}
	for _, path := range paths {
		stats.FilesScanned++
		name := nameOf(path)
		info, err := os.Stat(path)
		if err != nil {
			continue // 会话可能在扫描期间被归档或清理，下次同步补上
		}
		size := info.Size()
		var offset int64
		state := &fileState{}
		if row, ok := states[name]; ok {
			offset = row.Int("offset")
			if s := row.Str("state"); s != "" {
				if err := json.Unmarshal([]byte(s), state); err != nil {
					return err
				}
			}
		}
		if size == offset {
			continue
		}
		if size < offset { // 文件被重写，从头再读；唯一键保证不重复计数
			offset, state = 0, &fileState{}
		}
		stats.FilesRead++
		offset, n, err := ingest(ctx, path, name, offset, state)
		if err != nil {
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				continue
			}
			return err
		}
		stats.EventsAdded += n
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if _, err := ctx.DB.Exec("INSERT OR REPLACE INTO files VALUES (?, ?, ?, ?)", src.Name(), name, offset, string(encoded)); err != nil {
			return err
		}
	}
	if finish != nil {
		return finish(ctx)
	}
	return nil
}

func eventFromRow(r Row) *Event {
	return &Event{
		Source: r.Str("source"), Ukey: r.Str("ukey"), Ts: r.Float("ts"), File: r.Str("file"), Model: r.StrP("model"),
		Tier: r.Str("tier"), InputTokens: r.Int("input_tokens"), CachedInputTokens: r.Int("cached_input_tokens"),
		CacheWrite5mTokens: r.Int("cache_write_5m_tokens"), CacheWrite1hTokens: r.Int("cache_write_1h_tokens"),
		OutputTokens: r.Int("output_tokens"), ReasoningTokens: r.Int("reasoning_tokens"), LimitID: r.StrP("limit_id"),
		CycleResets: r.IntP("cycle_resets"), Cost: r.FloatP("cost"), PriceID: r.IntP("price_id"),
		Agent: r.StrP("agent"), Effort: r.StrP("effort"), Origin: r.StrP("origin"), Seq: r.Int("seq"),
	}
}

// RepriceAll 按最新价格表重算来源全部请求的金额。
func (ctx *Context) RepriceAll(src Source) error {
	return ctx.reprice(src, "SELECT * FROM events WHERE source=?", false)
}

// RepriceMissing 只给缺价的请求补算金额。
func (ctx *Context) RepriceMissing(src Source) error {
	return ctx.reprice(src, "SELECT * FROM events WHERE source=? AND cost IS NULL", true)
}

func (ctx *Context) reprice(src Source, query string, onlyPriced bool) error {
	book, err := ctx.Book(src, true)
	if err != nil {
		return err
	}
	return ctx.DB.Tx(func() error {
		rows, err := ctx.DB.Query(query, src.Name())
		if err != nil {
			return err
		}
		for _, r := range rows {
			amount, pid := book.Cost(eventFromRow(r))
			if onlyPriced && amount == nil {
				continue
			}
			if _, err := ctx.DB.Exec("UPDATE events SET cost=?, price_id=? WHERE id=?", nullable(amount), nullableInt(pid), r.Int("id")); err != nil {
				return err
			}
		}
		return nil
	})
}
