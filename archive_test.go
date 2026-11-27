package main

import (
	"fmt"
	"maps"
	"slices"
	"testing"
)

func TestArchivedAccountLeavesOnlyCurrentReports(t *testing.T) {
	cases := []struct {
		name   string
		argv   []string
		listed bool
	}{
		{"status", []string{"status"}, false},
		{"daily", []string{"daily"}, false},
		{"history", []string{"history"}, true},
		{"点名", []string{"status", "--source", "claude"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newTestContext(t)
			cliWriteSession(t, ctx, "claude-opus-5") // 请求在 2026-09-02
			writeFile(t, ctx.Args.ConfigPath, `{"archived":{"claude":"2026-09-10"}}`)
			out := cliRun(t, ctx, append(tc.argv, "--offline")...)
			block, ok := cliSources(t, out)["claude"].(map[string]any)
			if ok != tc.listed {
				t.Fatalf("列出 claude = %v，期望 %v: %v", ok, tc.listed, slices.Collect(maps.Keys(cliSources(t, out))))
			}
			if ok && block["archived"] != "2026-09-10" {
				t.Errorf("archived = %v", block["archived"])
			}
		})
	}
}

func TestArchivedEntryThatNoLongerFitsWarns(t *testing.T) {
	ctx := newTestContext(t)
	cliWriteSession(t, ctx, "claude-opus-5")
	writeFile(t, ctx.Args.ConfigPath, `{"archived":{"claude":"2026-09-01","gone":"2026-09-01"}}`)
	ws := cliWarnings(t, cliRun(t, ctx, "daily", "--offline"))
	for _, want := range []string{
		fmt.Sprintf("claude: 已归档（%s），这天起仍有请求或读数；核对账号归属，或从配置的 archived 里删掉", "2026-09-01"),
		"gone: 配置里归档了它，但没有这个账号的数据",
	} {
		if !slices.Contains(ws, want) {
			t.Errorf("warnings %q 缺少 %q", ws, want)
		}
	}
}
