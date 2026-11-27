// 归档账号：配置文件的 archived 写明不再使用的账号与归档日期。现状报表（status、daily、segments）不列它们，
// history 与 --source 点名时照常列出；数据与同步不受影响。
package main

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// archivedAccount 是一个归档账号：date 是配置里写的本地日期，since 是那天零点。
type archivedAccount struct {
	date  string
	since float64
}

// archivedSince 解析配置里的归档账号。
func archivedSince(args *Args, cfg *config) (map[string]archivedAccount, error) {
	out := map[string]archivedAccount{}
	for name, date := range cfg.Archived {
		day, err := time.ParseInLocation("2006-01-02", date, time.Local)
		if err != nil {
			return nil, fmt.Errorf("%s: archived.%s 不是 YYYY-MM-DD 日期: %q", args.ConfigPath, name, date)
		}
		out[name] = archivedAccount{date, float64(day.Unix())}
	}
	return out, nil
}

// reportNames 是这次报表列出的账号：--source 点名的照列；否则全部账号，history 以外去掉归档的。
func reportNames(args *Args, archived map[string]archivedAccount) []string {
	if len(args.Sources) > 0 {
		return args.Sources
	}
	names := sourceNames()
	if args.Command == "history" || args.Command == "prices" {
		return names
	}
	return slices.DeleteFunc(names, func(name string) bool {
		_, ok := archived[name]
		return ok
	})
}

// warnArchivedInUse：归档条目对不上现状时记警告，免得它悄悄藏掉还在用的账号。
func warnArchivedInUse(db *DB, ctx *Context, archived map[string]archivedAccount) error {
	for _, name := range slices.Sorted(maps.Keys(archived)) {
		if sources[name] == nil {
			ctx.warn(fmt.Sprintf("%s: 配置里归档了它，但没有这个账号的数据", name))
			continue
		}
		since := archived[name].since
		hit, err := db.QueryRow("SELECT 1 WHERE EXISTS (SELECT 1 FROM events WHERE source=?1 AND ts>=?2) "+
			"OR EXISTS (SELECT 1 FROM readings WHERE source=?1 AND ts>=?2)", name, since)
		if err != nil {
			return err
		}
		if hit != nil {
			ctx.warn(fmt.Sprintf("%s: 已归档（%s），这天起仍有请求或读数；核对账号归属，或从配置的 archived 里删掉",
				name, archived[name].date))
		}
	}
	return nil
}
