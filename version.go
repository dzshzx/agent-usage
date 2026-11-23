package main

import "runtime/debug"

const modulePath = "github.com/dzshzx/agent-usage"

// version 可在构建时经 -ldflags "-X main.version=…" 注入（发版脚本这样做）；没注入时取构建信息里本模块的版本。
var version = ""

// buildVersion 是 agent-usage version 打印的版本：注入的优先；go install …@版本 时是主模块的版本，作为别的模块的
// 依赖编出来时（go tool、go build github.com/dzshzx/agent-usage）是依赖里本模块的版本；都没有时是 dev。
func buildVersion(injected string) string {
	if injected != "" {
		return injected
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	for _, m := range append([]*debug.Module{&info.Main}, info.Deps...) {
		if m.Path != modulePath {
			continue
		}
		if m.Replace != nil {
			m = m.Replace
		}
		if m.Version != "" && m.Version != "(devel)" {
			return m.Version
		}
	}
	return "dev"
}
