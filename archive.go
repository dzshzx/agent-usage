// 归档账号：配置文件的 archived 写明不再使用的账号与归档日期。现状报表（status、daily、segments）不列它们，
// history 与 --source 点名时照常列出；数据与同步不受影响。
package main

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// reportNames 是这次报表列出的账号：--source 点名的照列；否则全部账号，history 以外去掉归档的。
// archived 是配置里的归档账号 → 归档日期（本地日期）。
func reportNames(args *Args, archived map[string]string) []string {
	if len(args.Sources) > 0 {
		return args.Sources
	}
	names := sourceNames()
	if args.Command == "history" {
		return names
	}
	return slices.DeleteFunc(names, func(name string) bool {
		_, ok := archived[name]
		return ok
	})
}

// warnArchivedInUse：归档条目对不上现状时记警告，免得它悄悄藏掉还在用的账号；归档日期写得不对时报错。
func warnArchivedInUse(db *DB, ctx *Context, archived map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(archived)) {
		day, err := time.ParseInLocation("2006-01-02", archived[name], time.Local)
		if err != nil {
			return fmt.Errorf("%s: archived.%s 不是 YYYY-MM-DD 日期: %q", ctx.Args.ConfigPath, name, archived[name])
		}
		if sources[name] == nil {
			ctx.warn(fmt.Sprintf("%s: 配置里归档了它，但没有这个账号的数据", name))
			continue
		}
		since := float64(day.Unix())
		hit, err := db.QueryRow("SELECT 1 WHERE EXISTS (SELECT 1 FROM events WHERE source=?1 AND ts>=?2) "+
			"OR EXISTS (SELECT 1 FROM readings WHERE source=?1 AND ts>=?2)", name, since)
		if err != nil {
			return err
		}
		if hit != nil {
			ctx.warn(fmt.Sprintf("%s: 已归档（%s），这天起仍有请求或读数；核对账号归属，或从配置的 archived 里删掉",
				name, archived[name]))
		}
	}
	return nil
}
