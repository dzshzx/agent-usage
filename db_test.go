// SQLite 封装的显式事务：成功提交、出错整体回滚、不能嵌套；事务外的语句各自提交。旧库的升级。
package main

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"testing"
)

func TestTxCommitsOrRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := openSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := createSchema(db); err != nil {
		t.Fatal(err)
	}
	insert := func(name string) error {
		_, err := db.Exec("INSERT INTO files VALUES ('s', ?, 0, NULL)", name)
		return err
	}
	if err := insert("outside"); err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(func() error { return insert("kept") }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := db.Tx(func() error {
		if err := insert("dropped"); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("Tx err = %v，期望 boom", err)
	}
	nested := db.Tx(func() error { return db.Tx(func() error { return insert("nested") }) })
	if nested == nil {
		t.Fatal("嵌套的 Tx 应报错")
	}
	// 另开一条连接读：只看得到已提交的行
	other, err := openSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var got []string
	rows, err := other.Query("SELECT name FROM files ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		got = append(got, r.Str("name"))
	}
	if len(got) != 2 || got[0] != "kept" || got[1] != "outside" {
		t.Errorf("已提交的行 = %v，期望 [kept outside]", got)
	}
}

// 事务先读后写时，另一个进程在两者之间提交了写入：Tx 一开始就占写锁，另一边按 busy_timeout 等待或报忙，
// 本事务的写入照常成功（两个会话同时跑 agent-usage 的情形）。
func TestTxWritesAfterConcurrentCommit(t *testing.T) {
	dir := t.TempDir()
	a, err := openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	b, err := openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	if _, err := b.Exec("PRAGMA busy_timeout=50"); err != nil {
		t.Fatal(err)
	}
	err = a.Tx(func() error {
		if _, err := a.Query("SELECT * FROM files"); err != nil {
			return err
		}
		b.Exec("INSERT INTO files VALUES ('s', 'b', 0, NULL)") // 另一个进程的写入：等不到锁就报忙，结果不管
		_, err := a.Exec("INSERT INTO files VALUES ('s', 'a', 0, NULL)")
		return err
	})
	if err != nil {
		t.Fatalf("Tx = %v，期望先读后写的事务照常提交", err)
	}
}

// 旧库升级：唯一约束写在表定义里的 events 重建成唯一索引，数据不丢；同一消息记了两行的留最早的一行；
// 没有序号的请求编在已有序号之后，读数补上序号列；升级后另一个账号再记同一消息不会多出一行。
func TestOpenDBMigratesOldEventKey(t *testing.T) {
	dir := t.TempDir()
	old, err := openSQLite(filepath.Join(dir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	ddl := strings.Replace(eventsTable, "    seq INTEGER\n);", "    seq INTEGER,\n    UNIQUE (source, ukey)\n);", 1)
	if ddl == eventsTable {
		t.Fatal("没能造出旧表定义")
	}
	err = old.ExecScript(schema + "DROP TABLE events; DROP TABLE readings;" + ddl + `CREATE INDEX events_ts ON events (source, ts);
CREATE TABLE readings (id INTEGER PRIMARY KEY, source TEXT NOT NULL, limit_id TEXT NOT NULL, window_minutes INTEGER NOT NULL,
    ts REAL NOT NULL, used_percent REAL NOT NULL, resets_at INTEGER NOT NULL, plan TEXT, dropped_from REAL, origin TEXT,
    UNIQUE (source, limit_id, ts, used_percent, resets_at));
INSERT INTO events (source, ukey, ts, file, tier, input_tokens, cached_input_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
    output_tokens, reasoning_tokens, seq) VALUES
    ('claude', 'a', 1, 'f', 'standard', 1, 0, 0, 0, 1, 0, 7),
    ('claude', 'b', 2, 'f', 'standard', 1, 0, 0, 0, 1, 0, NULL),
    ('claude-b', 'a', 3, 'f', 'standard', 1, 0, 0, 0, 1, 0, NULL),
    ('claude', 'c', 4, 'f', 'standard', 1, 0, 0, 0, 1, 0, NULL);
INSERT INTO readings (source, limit_id, window_minutes, ts, used_percent, resets_at) VALUES ('claude', 'seven_day', 10080, 1, 5, 100);`)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var got []string
	rows, err := db.Query("SELECT id, source, ukey, seq FROM events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%d:%s:%s:%d", r.Int("id"), r.Str("source"), r.Str("ukey"), r.Int("seq")))
	}
	if want := "1:claude:a:7,2:claude:b:9,4:claude:c:11"; strings.Join(got, ",") != want {
		t.Errorf("升级后的请求 = %s，want %s", strings.Join(got, ","), want)
	}
	if r, err := db.QueryRow("SELECT seq FROM readings"); err != nil || r.Int("seq") != 1 {
		t.Errorf("读数的序号 = %v %v", r, err)
	}
	for _, name := range []string{"sqlite_autoindex_events_1", "events_ukey", "events_ts", "events_origin_seq"} {
		v, err := db.Scalar("SELECT COUNT(*) FROM sqlite_master WHERE name=?", name)
		if want := int64(1); name == "sqlite_autoindex_events_1" {
			want = 0
			if toInt(v) != want || err != nil {
				t.Errorf("%s 还在", name)
			}
		} else if toInt(v) != want || err != nil {
			t.Errorf("缺索引 %s", name)
		}
	}
	if _, err := db.Exec("INSERT INTO events (source, ukey, ts, file, tier, input_tokens, cached_input_tokens, " +
		"cache_write_5m_tokens, cache_write_1h_tokens, output_tokens, reasoning_tokens) VALUES " +
		"('claude-b', 'b', 5, 'f', 'standard', 1, 0, 0, 0, 1, 0)"); err == nil {
		t.Error("同一消息能在另一个账号下再记一行")
	}
}

// 打开上下文时先对齐计价规则，再入库：新库直接记上当前版本（重算是空的）；版本旧的库先按新规则整体重算。
func TestOpenContextAlignsPricingRules(t *testing.T) {
	ctx := newTestContext(t)
	args := *ctx.Args
	fresh := args
	fresh.DataDir = t.TempDir()
	db, _, err := openContext(&fresh, false)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := db.userVersion(); err != nil || v != pricingRulesVersion {
		t.Errorf("新库的计价规则版本 = %d %v，want %d", v, err, pricingRulesVersion)
	}
	db.Close()

	syncAddEvent(t, ctx, "claude", "m", 60)
	if _, err := ctx.DB.Exec("UPDATE events SET cost=0"); err != nil {
		t.Fatal(err)
	}
	if err := ctx.DB.setUserVersion(0); err != nil {
		t.Fatal(err)
	}
	ctx.DB.Close()
	db, _, err = openContext(&args, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, err := db.userVersion(); err != nil || v != pricingRulesVersion {
		t.Errorf("重算后的计价规则版本 = %d %v，want %d", v, err, pricingRulesVersion)
	}
	if cost, err := db.Scalar("SELECT cost FROM events"); err != nil || toFloat(cost) == 0 {
		t.Errorf("版本旧的库没有重算金额: %v %v", cost, err)
	}
}

// eventFields 加上 id 正好是 events 表的全部列：写入与迁移都按它列列，漏一列迁移时会把那一列丢掉。
func TestEventFieldsCoverTable(t *testing.T) {
	ctx := newTestContext(t)
	cols, err := tableColumns(ctx.DB, "events")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"id": true}
	for _, f := range eventFields {
		want[f] = true
	}
	if !maps.Equal(cols, want) {
		t.Errorf("events 的列 %v，eventFields 加 id 是 %v", cols, want)
	}
}
