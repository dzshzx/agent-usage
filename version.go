package main

import "runtime/debug"

// version 可在构建时经 -ldflags "-X main.version=…" 注入（发版脚本这样做）；没注入时取构建信息里的主模块版本。
var version = ""

// buildVersion 是 agent-usage version 打印的版本：注入的优先；go install …@版本 时是主模块的版本；都没有时是 dev。
func buildVersion(injected string) string {
	if injected != "" {
		return injected
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
