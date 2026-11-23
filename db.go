// SQLite 连接封装：单连接、显式事务、按列名取值的行，拒绝迁移前的旧库，给已有的库补新列。
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// DB 持有一条 SQLite 连接。语句各自提交，要一起生效或一起作废的写入放进 Tx；查询结果整体读入内存后再返回。
type DB struct {
	conn  *sql.Conn
	db    *sql.DB
	inTx  bool
	stmts map[string]*sql.Stmt // 按 SQL 文本缓存的预编译语句：入库时同几条语句各执行几十万次
	// seq 是事务内已发出的本机最大序号；seqLoaded 为假时下次 nextSeq 先从库里读（每个事务开始时清掉）
	seq       int64
	seqLoaded bool
}

func openSQLite(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(60000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	return &DB{conn: conn, db: sqlDB, stmts: map[string]*sql.Stmt{}}, nil
}

// stmt 返回 query 的预编译语句，首次用到时在本连接上编译并缓存。
func (d *DB) stmt(query string) (*sql.Stmt, error) {
	if s, ok := d.stmts[query]; ok {
		return s, nil
	}
	s, err := d.conn.PrepareContext(context.Background(), query)
	if err != nil {
		return nil, err
	}
	d.stmts[query] = s
	return s, nil
}

// openDB 打开（必要时新建）数据目录里的 usage.db：给旧库补列、升级表结构，再建表与索引。
func openDB(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "usage.db")
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}
	for _, step := range []func(*DB) error{addColumns, migrateEventKey, createSchema} {
		if err := step(db); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

// createSchema 建表与索引，已有的跳过。
func createSchema(db *DB) error {
	if err := db.ExecScript(schema); err != nil {
		return err
	}
	return createEventIndexes(db)
}

func createEventIndexes(db *DB) error {
	for _, idx := range eventIndexes {
		if _, err := db.Exec(idx.ddl); err != nil {
			return err
		}
	}
	return nil
}

// dropBulkIndexes 删掉批量导入时不逐行维护的 events 索引，导完由 createEventIndexes 重建。
func dropBulkIndexes(db *DB) error {
	for _, idx := range eventIndexes {
		if idx.rebuild {
			if _, err := db.Exec("DROP INDEX " + idx.name); err != nil {
				return err
			}
		}
	}
	return nil
}

// migrateEventKey：旧库的唯一约束 (source, ukey) 写在表定义里，删不掉；重建一次 events 表，改由 eventIndexes 里的唯一索引
// 保证一条消息只记一次（同一消息记了不止一行的留最早的一行）。还没有序号的行（旧版本写入的）顺带编上序号，排在已有的之后；
// 旧表上的索引随旧表删掉，按对端记游标的旧表也一并删掉（进度改由各出处的序号推出）。重建在一个事务里，中途出错时库保持原样；
// 索引由之后的 createSchema 建。
func migrateEventKey(db *DB) error {
	old, err := db.Scalar("SELECT 1 FROM sqlite_master WHERE name='sqlite_autoindex_events_1'")
	if err != nil || old == nil {
		return err
	}
	sel := make([]string, len(eventFields))
	for i, f := range eventFields {
		if sel[i] = f; f == "seq" {
			sel[i] = "COALESCE(seq, (SELECT COALESCE(MAX(seq), 0) FROM events_old) + id)"
		}
	}
	return db.Tx(func() error {
		return db.ExecScript("ALTER TABLE events RENAME TO events_old;" + eventsTable +
			"INSERT INTO events (id, " + eventColumns + ") SELECT id, " + strings.Join(sel, ", ") + " FROM events_old " +
			"WHERE id IN (SELECT MIN(id) FROM events_old GROUP BY ukey); DROP TABLE events_old; DROP TABLE IF EXISTS sync_peers;")
	})
}

// ownSeq 是本机已发出的最大序号：本机入库（origin 为空）的请求与读数共用一个序号空间。
func (d *DB) ownSeq() (int64, error) {
	v, err := d.Scalar("SELECT MAX(COALESCE((SELECT MAX(seq) FROM events WHERE origin IS NULL), 0), " +
		"COALESCE((SELECT MAX(seq) FROM readings WHERE origin IS NULL), 0))")
	return toInt(v), err
}

// nextSeq 发一个本机序号：本机日志入库的请求新增或补全、读数新增时记上，同步按它判断对端缺哪些。
// 序号取「上一个加一」与「当前微秒时刻」中较大的（混合逻辑时钟）：库从备份恢复后新写的行，序号也大于恢复前发出过的，
// 对端不会以为已经有了而漏收。写入都在 BEGIN IMMEDIATE 的事务里，同一时间只有一个进程在发号；事务外每次都从库里读上一个。
func (d *DB) nextSeq() (int64, error) {
	if !d.seqLoaded || !d.inTx {
		v, err := d.ownSeq()
		if err != nil {
			return 0, err
		}
		d.seq, d.seqLoaded = v, d.inTx
	}
	d.seq = max(d.seq+1, int64(now()*1e6))
	return d.seq, nil
}

func (d *DB) Close() error {
	for _, s := range d.stmts {
		s.Close()
	}
	d.stmts = map[string]*sql.Stmt{}
	err := d.conn.Close()
	if e := d.db.Close(); err == nil {
		err = e
	}
	return err
}

// Exec 执行一条语句；在 Tx 外执行时立即提交。
func (d *DB) Exec(query string, args ...any) (sql.Result, error) {
	s, err := d.stmt(query)
	if err != nil {
		return nil, err
	}
	return s.ExecContext(context.Background(), args...)
}

// ExecScript 执行多条语句的脚本（建表）。
func (d *DB) ExecScript(script string) error {
	_, err := d.conn.ExecContext(context.Background(), script)
	return err
}

// Tx 在一个事务里执行 fn：fn 返回 nil 时提交；fn 出错或提交失败时回滚，返回原错误。不能嵌套。
// 用 BEGIN IMMEDIATE 一开始就占写锁（另一个进程正在写时按 busy_timeout 等待）：fn 先读后写，
// 普通 BEGIN 下读完之后别的进程提交过，写入会不等待直接报 database is locked。
func (d *DB) Tx(fn func() error) error {
	if d.inTx {
		return errors.New("事务不能嵌套")
	}
	bg := context.Background()
	if _, err := d.conn.ExecContext(bg, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	d.inTx, d.seqLoaded = true, false
	defer func() { d.inTx = false }()
	err := fn()
	if err == nil {
		if _, err = d.conn.ExecContext(bg, "COMMIT"); err == nil {
			return nil
		}
	}
	if _, rbErr := d.conn.ExecContext(bg, "ROLLBACK"); rbErr != nil {
		return errors.Join(err, rbErr)
	}
	return err
}

// Row 是按列名取值的一行；INTEGER 为 int64，REAL 为 float64，TEXT 为 string，NULL 为 nil。
type Row map[string]any

// Query 执行查询并返回全部行。
func (d *DB) Query(query string, args ...any) ([]Row, error) {
	s, err := d.stmt(query)
	if err != nil {
		return nil, err
	}
	rows, err := s.QueryContext(context.Background(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []Row
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := make(Row, len(cols))
		for i, c := range cols {
			r[c] = vals[i]
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Each 执行查询，逐行调用 fn，fn 用 scan 按 SELECT 的列序取值；结果不整体读入内存，适合几十万行的导出。
func (d *DB) Each(query string, args []any, fn func(scan func(dest ...any) error) error) error {
	s, err := d.stmt(query)
	if err != nil {
		return err
	}
	rows, err := s.QueryContext(context.Background(), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

// QueryRow 返回第一行；没有结果时为 nil。
func (d *DB) QueryRow(query string, args ...any) (Row, error) {
	rows, err := d.Query(query, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// Scalar 返回第一行第一列（按 SELECT 列顺序取不到时按列名唯一的那一列）。
func (d *DB) Scalar(query string, args ...any) (any, error) {
	s, err := d.stmt(query)
	if err != nil {
		return nil, err
	}
	rows, err := s.QueryContext(context.Background(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var v any
	if err := rows.Scan(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func (r Row) IsNull(k string) bool { return r[k] == nil }

func (r Row) Str(k string) string {
	s, _ := r[k].(string)
	return s
}

func (r Row) StrP(k string) *string {
	if s, ok := r[k].(string); ok {
		return &s
	}
	return nil
}

// Float 把 INTEGER 或 REAL 列读成 float64；NULL 为 0。
func (r Row) Float(k string) float64 {
	return toFloat(r[k])
}

func (r Row) FloatP(k string) *float64 {
	if r[k] == nil {
		return nil
	}
	return floatp(toFloat(r[k]))
}

// Int 把 INTEGER 或 REAL 列读成 int64；NULL 为 0。
func (r Row) Int(k string) int64 {
	return toInt(r[k])
}

func (r Row) IntP(k string) *int64 {
	if r[k] == nil {
		return nil
	}
	return intp(toInt(r[k]))
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case int:
		return int64(x)
	}
	return 0
}

func scalarFloatP(v any) *float64 {
	if v == nil {
		return nil
	}
	return floatp(toFloat(v))
}

// userVersion 读写 PRAGMA user_version。
func (d *DB) userVersion() (int64, error) {
	v, err := d.Scalar("PRAGMA user_version")
	return toInt(v), err
}

func (d *DB) setUserVersion(v int64) error {
	_, err := d.Exec(fmt.Sprintf("PRAGMA user_version=%d", v))
	return err
}

// meta 读 meta 表里 key 的值；没有时为 nil。
func (d *DB) meta(key string) (*string, error) {
	v, err := d.Scalar("SELECT value FROM meta WHERE key=?", key)
	if err != nil || v == nil {
		return nil, err
	}
	s := jsonText(v)
	return &s, nil
}

func (d *DB) setMeta(key, value string) error {
	_, err := d.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)", key, value)
	return err
}

// placeholders 是 n 个以逗号隔开的 ?。
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// tableColumns 返回表的列名集合；表不存在时为空。
func tableColumns(db *DB, table string) (map[string]bool, error) {
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	cols := make(map[string]bool, len(rows))
	for _, r := range rows {
		cols[r.Str("name")] = true
	}
	return cols, nil
}

// addedColumns 是打开已有的库时补上的列；fill 非空时在补上列之后执行一次回填，否则空值就是正确的初始值。
// schema 里它们排在表的末尾，与 ALTER TABLE 补出来的位置一致。events 的序号由 migrateEventKey 重建表时补上。
// 补上的序号都远小于混合逻辑时钟（见 nextSeq）之后发的号：旧版本同步来的行在这里编的是本机序号，
// 对端按序号判断缺不缺时，这些行只在对端完全没见过对应出处时才会再传。
var addedColumns = []struct{ table, column, decl, fill string }{
	{"prices", "long_cache_write_1h", "REAL", ""},
	{"events", "origin", "TEXT", ""},
	{"events", "seq", "INTEGER", ""},
	{"readings", "origin", "TEXT", ""},
	{"readings", "seq", "INTEGER", "UPDATE readings SET seq = id"},
}

// addColumns 给缺 addedColumns 里的列的库补上这些列；还没有的表跳过，之后由 createSchema 按完整定义建。
func addColumns(db *DB) error {
	for _, c := range addedColumns {
		cols, err := tableColumns(db, c.table)
		if err != nil {
			return err
		}
		if len(cols) == 0 || cols[c.column] {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.decl)); err != nil {
			return err
		}
		if c.fill != "" {
			if _, err := db.Exec(c.fill); err != nil {
				return err
			}
		}
	}
	return nil
}
