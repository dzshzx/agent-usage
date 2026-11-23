// 多台机器之间同步：消息的往返、按版本向量只发对方缺的、去重、账号编号核对，以及 sync 的多台累计与自动同步的间隔。
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// syncEvent 是一条 7 天窗口的请求（输出 10 个 token）；ago 是距现在的秒数。
func syncEvent(ukey string, ago float64) *Event {
	ev := testEvent(ukey, now()-ago, "s.jsonl", "claude-opus-5", weekLimitID, 1000)
	ev.OutputTokens = 10
	return ev
}

// syncAddEvent 在 ctx 的库里记一条本机入库的 syncEvent。
func syncAddEvent(t *testing.T, ctx *Context, account, ukey string, ago float64) {
	t.Helper()
	mustAddEvent(t, ctx, accountSource(account), syncEvent(ukey, ago))
}

// sent 是一条消息里的请求行数与读数行数。
type sent struct{ Events, Readings int }

// syncMessage 是 ctx 发给向量为 theirs 的对端的消息（theirs 为空时是整份），以及其中的行数。
func syncMessage(t *testing.T, ctx *Context, theirs map[string]int64) (*bytes.Buffer, sent) {
	t.Helper()
	var buf bytes.Buffer
	if err := writeMessage(ctx, newFrameWriter(&buf), theirs, true); err != nil {
		t.Fatal(err)
	}
	var n sent
	for _, line := range strings.Split(syncPlain(t, buf.Bytes()), "\n") {
		switch {
		case strings.HasPrefix(line, `["e",`):
			n.Events++
		case strings.HasPrefix(line, `["r",`):
			n.Readings++
		}
	}
	return &buf, n
}

func syncImport(t *testing.T, ctx *Context, data []byte) *importStats {
	t.Helper()
	stats, _, err := readMessage(ctx, newFrameReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func syncVector(t *testing.T, ctx *Context) map[string]int64 {
	t.Helper()
	_, vec, err := vector(ctx.DB)
	if err != nil {
		t.Fatal(err)
	}
	return vec
}

// syncPlain 解压一条消息，返回其中的 JSON Lines。
func syncPlain(t *testing.T, data []byte) string {
	t.Helper()
	b, err := io.ReadAll(newFrameReader(bytes.NewReader(data)).message())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// syncUkeys 是库里某账号的全部消息 id，用逗号连起来（按 id 排序）。
func syncUkeys(t *testing.T, ctx *Context, account string) string {
	t.Helper()
	var keys []string
	for _, r := range mustQuery(t, ctx, "SELECT ukey FROM events WHERE source=? ORDER BY ukey", account) {
		keys = append(keys, r.Str("ukey"))
	}
	return strings.Join(keys, ",")
}

// 整份往返：各账号（含 claude-switch add 加的号）的请求与读数原样过去，字段逐一还原，金额由收方重算；
// 非账号目标与旧库别的限额名不发；同一份再收一次不重复计数。
func TestMessageRoundTrip(t *testing.T) {
	a := newTestContext(t)
	ev := testEvent("m1", 1790000000.123, "proj/s.jsonl", "claude-opus-5", weekLimitID, 1500)
	ev.CachedInputTokens, ev.CacheWrite5mTokens, ev.CacheWrite1hTokens = 1000, 100, 200
	ev.OutputTokens, ev.ReasoningTokens, ev.Agent, ev.Effort = 30, 7, strp("Explore"), strp("high")
	mustAddEvent(t, a, accountSource("claude"), ev)
	syncAddEvent(t, a, "work", "m2", 7200)
	syncAddEvent(t, a, targetSource, "m_target", 7200)
	mustAddEvent(t, a, accountSource("claude"), testEvent("m_codex", now()-7200, "f", "gpt", "codex", 10))
	if _, err := a.AddReading("work", weekLimitID, weekMinutes, 1790000100, 12.5, 1790442000, floatp(40)); err != nil {
		t.Fatal(err)
	}
	data, end := syncMessage(t, a, map[string]int64{})
	if end.Events != 2 || end.Readings != 1 {
		t.Fatalf("发出 %d 条请求、%d 条读数，want 2 与 1", end.Events, end.Readings)
	}

	b := newTestContext(t)
	stats := syncImport(t, b, data.Bytes())
	if stats.EventsAdded != 2 || stats.ReadingsAdded != 1 || stats.Received != 3 {
		t.Errorf("导入结果 %+v", stats)
	}
	got := mustQuery(t, b, "SELECT * FROM events WHERE ukey='m1'")[0]
	for col, want := range map[string]any{"ts": 1790000000.123, "file": "proj/s.jsonl", "input_tokens": int64(1500),
		"cached_input_tokens": int64(1000), "cache_write_5m_tokens": int64(100), "cache_write_1h_tokens": int64(200),
		"output_tokens": int64(30), "reasoning_tokens": int64(7), "agent": "Explore", "effort": "high"} {
		if got[col] != want {
			t.Errorf("m1.%s = %v，want %v", col, got[col], want)
		}
	}
	if got.IsNull("cost") || got.IsNull("origin") {
		t.Errorf("m1 没有按收方价格表算金额或没记出处: %v", got)
	}
	r := mustQuery(t, b, "SELECT * FROM readings")[0]
	if r.Str("source") != "work" || r.Float("used_percent") != 12.5 || r.Float("dropped_from") != 40 {
		t.Errorf("读数 = %v", r)
	}
	if n := count(t, b, "events", "source='@target' OR ukey='m_codex'"); n != 0 {
		t.Errorf("非账号目标或别的限额名的请求被发了 %d 条", n)
	}
	again := syncImport(t, b, data.Bytes())
	if again.EventsAdded != 0 || again.ReadingsAdded != 0 || count(t, b, "events", "") != 2 {
		t.Errorf("再收一次: %+v，库里 %d 条", again, count(t, b, "events", ""))
	}
}

// 按向量只发对方缺的：收过的不再发；新增的与输出变多的（换了新序号）再发；对方向量为空（删库重建）时整份发。
func TestMessageSendsOnlyWhatPeerLacks(t *testing.T) {
	a, b := newTestContext(t), newTestContext(t)
	syncAddEvent(t, a, "claude", "old", 7200)
	syncAddEvent(t, a, "claude", "streaming", 60)
	data, _ := syncMessage(t, a, syncVector(t, b))
	syncImport(t, b, data.Bytes())
	if _, idle := syncMessage(t, a, syncVector(t, b)); idle.Events != 0 || idle.Readings != 0 {
		t.Errorf("对方都有了还发 %+v", idle)
	}
	syncAddEvent(t, a, "claude", "new", 30)
	grown := syncEvent("streaming", 60)
	grown.OutputTokens = 500
	mustAddEvent(t, a, accountSource("claude"), grown)
	data, end := syncMessage(t, a, syncVector(t, b))
	if plain := syncPlain(t, data.Bytes()); end.Events != 2 || strings.Contains(plain, `"old"`) {
		t.Errorf("增量发了 %d 条:\n%s", end.Events, plain)
	}
	syncImport(t, b, data.Bytes())
	if out := mustQuery(t, b, "SELECT output_tokens FROM events WHERE ukey='streaming'")[0].Int("output_tokens"); out != 500 {
		t.Errorf("补全后的输出 = %d，want 500", out)
	}
	if _, full := syncMessage(t, a, map[string]int64{}); full.Events != 3 {
		t.Errorf("向量为空时应整份 3 条，实际 %d 条", full.Events)
	}
}

// 格式紧凑：来源、文件、模型等重复的字符串只写一次。
func TestMessageInternsRepeatedStrings(t *testing.T) {
	a := newTestContext(t)
	for i := range 50 {
		syncAddEvent(t, a, "claude", fmt.Sprintf("m%d", i), 7200)
	}
	data, _ := syncMessage(t, a, map[string]int64{})
	if n := strings.Count(syncPlain(t, data.Bytes()), `"s.jsonl"`); n != 1 {
		t.Errorf("文件名写了 %d 次，want 1", n)
	}
	b := newTestContext(t)
	if stats := syncImport(t, b, data.Bytes()); stats.EventsAdded != 50 {
		t.Errorf("导入 %d 条，want 50", stats.EventsAdded)
	}
}

// 收方已把同一消息记在别的账号下时不再记，但仍算见过（不会下次再要）；本机日志里的同一消息不被外来的覆盖；
// 消息不完整、两边库编号相同或账号编号对不上时整份不写。
func TestImportRejectsAndDedupes(t *testing.T) {
	a := newTestContext(t)
	syncAddEvent(t, a, "claude", "dup", 7200)
	syncAddEvent(t, a, "claude", "fresh", 7200)
	grown := syncEvent("mine", 7200)
	grown.OutputTokens = 900
	mustAddEvent(t, a, accountSource("claude"), grown)
	data, _ := syncMessage(t, a, map[string]int64{})

	b := newTestContext(t)
	syncAddEvent(t, b, "claude-b", "dup", 7200)
	syncAddEvent(t, b, "claude", "mine", 7200)
	stats := syncImport(t, b, data.Bytes())
	if stats.EventsAdded != 1 || syncUkeys(t, b, "claude-b") != "dup" || syncUkeys(t, b, "claude") != "fresh,mine" {
		t.Errorf("导入结果 %+v，claude-b = %q，claude = %q", stats, syncUkeys(t, b, "claude-b"), syncUkeys(t, b, "claude"))
	}
	if r := mustQuery(t, b, "SELECT output_tokens, origin FROM events WHERE ukey='mine'")[0]; r.Int("output_tokens") != 10 || !r.IsNull("origin") {
		t.Errorf("外来的行改了本机的行: %v", r)
	}
	if _, rest := syncMessage(t, a, syncVector(t, b)); rest.Events != 0 {
		t.Errorf("去重没写入的行下次还要发 %d 条", rest.Events)
	}
	// 发方库里同一消息记在两个账号下（旧数据）：收方同一批里也只计一次
	twice := newTestContext(t)
	syncAddEvent(t, twice, "claude", "both", 7200)
	syncAddEvent(t, twice, "claude-b", "both", 7200)
	data2, _ := syncMessage(t, twice, map[string]int64{})
	if got := syncImport(t, newTestContext(t), data2.Bytes()); got.EventsAdded != 1 {
		t.Errorf("同一消息两个账号: %+v", got)
	}

	c := newTestContext(t)
	whole := data.Bytes()
	for _, cut := range []int{len(whole) - 4, len(whole) / 2} { // 少了结束帧；断在帧中间
		if _, _, err := readMessage(c, newFrameReader(bytes.NewReader(whole[:cut]))); err == nil || !strings.Contains(err.Error(), "不完整") {
			t.Errorf("截到 %d 字节: err = %v", cut, err)
		}
	}
	if n := count(t, c, "events", ""); n != 0 {
		t.Errorf("不完整的消息写进了 %d 条", n)
	}
	// 库是从发方拷贝来的（编号相同）：拒绝，否则两边都把对方的行当成自己的
	ownA, err := a.DB.instanceID()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DB.setMeta("instance", ownA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readMessage(c, newFrameReader(bytes.NewReader(whole))); err == nil || !strings.Contains(err.Error(), "库编号相同") {
		t.Errorf("编号相同: err = %v", err)
	}
	if err := c.DB.setMeta("instance", "c"); err != nil {
		t.Fatal(err)
	}

	writeJSONL(t, filepath.Join(a.Args.SwitchStateDir, "accounts.json"), obj{"claude": "uuid-1", "work": "uuid-w"})
	writeJSONL(t, filepath.Join(c.Args.SwitchStateDir, "accounts.json"), obj{"claude": "uuid-2"})
	data, _ = syncMessage(t, a, map[string]int64{})
	if _, _, err := readMessage(c, newFrameReader(data)); err == nil || !strings.Contains(err.Error(), "账号 claude 在两台机器上") {
		t.Errorf("账号编号不同: err = %v", err)
	}
	if n := count(t, c, "events", ""); n != 0 {
		t.Errorf("账号编号不同时写进了 %d 条", n)
	}
}

// 批量写入（删索引直接追加、最后重建）与逐批合并的结果相同，包括撞上已有消息 id 的行（另一个账号的、本机的、
// 先前收过、输出变多了的）；
// 之后索引都在。收的请求比库里原有的多时走批量写入，否则逐批合并。
func TestBulkImportMatchesMerge(t *testing.T) {
	sender := newTestContext(t)
	syncAddEvent(t, sender, "claude", "grow", 7200)
	early, _ := syncMessage(t, sender, map[string]int64{}) // 收方先收到输出还没写完的 grow
	grow := syncEvent("grow", 7200)
	grow.OutputTokens = 900
	mustAddEvent(t, sender, accountSource("claude"), grow)
	for i := range 5 {
		syncAddEvent(t, sender, "claude", fmt.Sprintf("m%d", i), 7200)
	}
	syncAddEvent(t, sender, "claude", "dup", 7200)
	grown := syncEvent("mine", 7200)
	grown.OutputTokens = 900
	mustAddEvent(t, sender, accountSource("claude"), grown)
	data, n := syncMessage(t, sender, map[string]int64{})

	dump := func(ctx *Context) string {
		var rows []string
		for _, r := range mustQuery(t, ctx, "SELECT source, ukey, output_tokens, origin IS NULL AS mine FROM events "+
			"WHERE source != 'pad' ORDER BY ukey") {
			rows = append(rows, fmt.Sprintf("%s:%s:%d:%d", r.Str("source"), r.Str("ukey"), r.Int("output_tokens"), r.Int("mine")))
		}
		return strings.Join(rows, ",")
	}
	run := func(pad int) (string, *importStats) {
		b := newTestContext(t)
		syncImport(t, b, early.Bytes())
		syncAddEvent(t, b, "claude-b", "dup", 7200)
		syncAddEvent(t, b, "claude", "mine", 7200)
		for i := range pad {
			syncAddEvent(t, b, "pad", fmt.Sprintf("pad%d", i), 7200)
		}
		stats := syncImport(t, b, data.Bytes())
		if n := count(t, b, "sqlite_master", "tbl_name='events' AND type='index' AND sql IS NOT NULL"); n != int64(len(eventIndexes)) {
			t.Errorf("pad=%d: 导入后 events 有 %d 个索引，want %d", pad, n, len(eventIndexes))
		}
		return dump(b), stats
	}
	want, wantStats := run(n.Events) // 原有的不比收的少：逐批合并
	if !strings.Contains(want, "claude-b:dup:10:1") || !strings.Contains(want, "claude:mine:10:1") ||
		!strings.Contains(want, "claude:grow:900:0") || wantStats.EventsAdded != 6 {
		t.Fatalf("逐批合并: %s %+v", want, wantStats)
	}
	if got, stats := run(0); got != want || stats.EventsAdded != wantStats.EventsAdded {
		t.Errorf("批量写入:\n%s %+v\nwant\n%s %+v", got, stats, want, wantStats)
	}
}

// 新库（没对齐过价格）先拉价格再导入：导入的请求当场有金额，不用等第一次出报表时整批补算；离线时照常导入、金额留空。
func TestImportIntoFreshDBPricesRowsFirst(t *testing.T) {
	a := newTestContext(t)
	syncAddEvent(t, a, "claude", "m1", 7200)
	data, _ := syncMessage(t, a, map[string]int64{})
	for _, offline := range []bool{false, true} {
		args := *a.Args
		args.DataDir, args.Offline = t.TempDir(), offline
		args.PricingURL = fileURL(fixturePath(t, "anthropic-pricing.md"))
		if stats, err := runImport(&args, bytes.NewReader(data.Bytes())); err != nil || stats.EventsAdded != 1 {
			t.Fatalf("offline=%v: %+v %v", offline, stats, err)
		}
		db, err := openDB(args.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		n, err := db.Scalar("SELECT COUNT(*) FROM events WHERE cost IS NOT NULL")
		db.Close()
		if want := map[bool]int64{false: 1, true: 0}[offline]; err != nil || toInt(n) != want {
			t.Errorf("offline=%v: 有金额的请求 %v 条，want %d（%v）", offline, n, want, err)
		}
	}
}

// fakePeers 把主机名接到各自的测试库上，代替 ssh：对端一边在协程里跑 servePeer。calls 记下每次连的主机。
type fakePeers struct {
	peers map[string]*Context
	calls []string
	down  map[string]bool
}

func (f *fakePeers) install(t *testing.T) {
	saved := remoteRun
	t.Cleanup(func() { remoteRun = saved })
	remoteRun = func(_ *Args, host string, args []string, session func(io.WriteCloser, io.Reader) error) error {
		f.calls = append(f.calls, host)
		if f.down[host] {
			return &unreachableError{msg: "ssh: connect to host " + host + ": No route to host"}
		}
		if args[0] != "peer" {
			return errors.New("sync 只该调用 peer")
		}
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		done := make(chan error, 1)
		go func() {
			err := servePeer(f.peers[host], inR, outW)
			outW.CloseWithError(err)
			inR.CloseWithError(err)
			done <- err
		}()
		err := session(inW, outR)
		inW.Close()
		outR.Close()
		if remote := <-done; err == nil {
			err = remote
		}
		return err
	}
}

func syncMust(t *testing.T, ctx *Context, host string) *peerSync {
	t.Helper()
	p, err := syncPeer(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 三台累计：A↔B、C↔B 之后 A 再与 B 同步，三台都有全部请求；之后两边都有同样数据时一行都不传，
// 包括从没直接同步过、各自经 B 拿到全部数据的 A 与 C。
func TestSyncAccumulatesAcrossThreeMachines(t *testing.T) {
	a, b, c := newTestContext(t), newTestContext(t), newTestContext(t)
	syncAddEvent(t, a, "claude", "from_a", 7200)
	syncAddEvent(t, b, "work", "from_b", 7200)
	syncAddEvent(t, c, "claude-b", "from_c", 7200)
	f := &fakePeers{peers: map[string]*Context{"a": a, "b": b, "c": c}}
	f.install(t)

	if p := syncMust(t, a, "b"); p.Pulled.EventsAdded != 1 || p.Pushed.EventsAdded != 1 {
		t.Errorf("A↔B: 拉 %+v 推 %+v", p.Pulled, p.Pushed)
	}
	if p := syncMust(t, a, "b"); p.Pulled.Received != 0 || p.Pushed.Received != 0 {
		t.Errorf("紧接着再同步还传了 拉 %d 行、推 %d 行", p.Pulled.Received, p.Pushed.Received)
	}
	syncMust(t, c, "b")
	syncMust(t, a, "b")
	for name, ctx := range map[string]*Context{"A": a, "B": b, "C": c} {
		got := syncUkeys(t, ctx, "claude") + "|" + syncUkeys(t, ctx, "claude-b") + "|" + syncUkeys(t, ctx, "work")
		if got != "from_a|from_c|from_b" {
			t.Errorf("%s 上的请求 = %q", name, got)
		}
	}
	if p := syncMust(t, a, "c"); p.Pulled.Received != 0 || p.Pushed.Received != 0 {
		t.Errorf("A、C 第一次直连就传了 拉 %d 行、推 %d 行", p.Pulled.Received, p.Pushed.Received)
	}
}

// 对端删库重建（向量为空）后本机的全部再发一遍；本机从备份恢复后，对端发回本机已没有的行，按本机的行收下；
// 之后新写的行，序号仍大于恢复前发出过的，对端照样收到。
func TestSyncRecoversFromRebuildAndRestore(t *testing.T) {
	a, b := newTestContext(t), newTestContext(t)
	syncAddEvent(t, a, "claude", "m1", 7200)
	syncAddEvent(t, a, "claude", "m2", 7200)
	f := &fakePeers{peers: map[string]*Context{"b": b}}
	f.install(t)
	syncMust(t, a, "b")

	rebuilt := newTestContext(t)
	f.peers["b"] = rebuilt
	if p := syncMust(t, a, "b"); p.Pushed.EventsAdded != 2 {
		t.Errorf("推给重建的库: %+v", p.Pushed)
	}

	if _, err := a.DB.Exec("DELETE FROM events WHERE ukey='m2'"); err != nil { // 恢复到只有 m1 的备份
		t.Fatal(err)
	}
	if p := syncMust(t, a, "b"); p.Pulled.EventsAdded != 1 {
		t.Errorf("恢复备份后没从对端拿回 m2: %+v", p.Pulled)
	}
	if r := mustQuery(t, a, "SELECT origin FROM events WHERE ukey='m2'"); len(r) != 1 || !r[0].IsNull("origin") {
		t.Errorf("拿回的 m2 应按本机的行收下: %v", r)
	}
	syncAddEvent(t, a, "claude", "m3", 60)
	syncMust(t, a, "b")
	if got := syncUkeys(t, rebuilt, "claude"); got != "m1,m2,m3" {
		t.Errorf("恢复备份后新写的行没传过去: 对端 = %q", got)
	}
}

// 自动同步：按上次尝试的时刻算到期，成败都算；离线的对端一个间隔里只试一次；连不上只记警告；0 关闭。
func TestAutoSyncRespectsInterval(t *testing.T) {
	a, b := newTestContext(t), newTestContext(t)
	syncAddEvent(t, b, "claude", "from_b", 7200)
	f := &fakePeers{peers: map[string]*Context{"b": b}, down: map[string]bool{"phone": true}}
	f.install(t)
	writeJSONL(t, a.Args.ConfigPath, obj{"peers": []string{"b", "phone"}, "auto_sync_minutes": 30})

	if err := autoSync(a); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, ";"); got != "b;phone" {
		t.Errorf("第一次连了 %q", got)
	}
	if len(a.Warnings) != 1 || !strings.Contains(a.Warnings[0], "同步 phone：连不上") {
		t.Errorf("警告 = %v", a.Warnings)
	}
	if syncUkeys(t, a, "claude") != "from_b" {
		t.Error("自动同步没有拉回对端的请求")
	}
	f.calls = nil
	if err := autoSync(a); err != nil || len(f.calls) != 0 {
		t.Errorf("30 分钟内再跑不该连任何对端，实际 %q %v", f.calls, err)
	}
	saved := now
	t.Cleanup(func() { now = saved })
	later := saved() + 31*60
	now = func() float64 { return later }
	if err := autoSync(a); err != nil || strings.Join(f.calls, ";") != "b;phone" {
		t.Errorf("过了间隔应再连 b 与 phone，实际 %q %v", f.calls, err)
	}
	writeJSONL(t, a.Args.ConfigPath, obj{"peers": []string{"b"}, "auto_sync_minutes": 0})
	f.calls = nil
	now = func() float64 { return later + 3600 }
	if err := autoSync(a); err != nil || len(f.calls) != 0 {
		t.Errorf("auto_sync_minutes=0 时仍连了对端: %v %v", f.calls, err)
	}
}

// sync 命令：先把本机会话日志里新的用量入库再推；不给主机时取配置里的对端；连不上只是警告（退出码 0），对端报错退出码 1。
func TestSyncCommandExitCodes(t *testing.T) {
	a, b := newTestContext(t), newTestContext(t)
	f := &fakePeers{peers: map[string]*Context{"b": b}, down: map[string]bool{"phone": true}}
	f.install(t)
	writeJSONL(t, a.Args.ConfigPath, obj{"peers": []string{"b", "phone"}})
	writeJSONL(t, filepath.Join(a.Args.ClaudeHome, "projects", "proj", "s.jsonl"),
		claudeMessage("m1", 10, isoOf(now()-3600), "claude-opus-5"))
	if err := a.DB.Close(); err != nil {
		t.Fatal(err)
	}
	base := append(cliPathArgs(a), "--text")
	var stdout, stderr bytes.Buffer
	if code := run(append([]string{"sync"}, base...), &stdout, &stderr); code != 0 {
		t.Fatalf("退出码 %d：%s%s", code, stdout.String(), stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "b：拉回请求 0 条、读数 0 条；推送请求 1 条") || !strings.Contains(out, "phone：没有同步完") {
		t.Errorf("输出:\n%s", out)
	}
	saved := remoteRun
	remoteRun = func(*Args, string, []string, func(io.WriteCloser, io.Reader) error) error {
		return errors.New("对端 agent-usage peer 失败（exit status 1）：agent-usage: database is locked")
	}
	defer func() { remoteRun = saved }()
	stdout.Reset()
	if code := run(append([]string{"sync", "b"}, base...), &stdout, &stderr); code != 1 {
		t.Errorf("对端报错时退出码 %d，want 1\n%s", code, stdout.String())
	}
}
