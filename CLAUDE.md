# agent-usage 项目约定

公开仓：提交说明、文档、注释与测试数据按公开可见来写，不写本机路径、私有域名、主机名、真实邮箱或令牌。测试只用假数据；推送前的 gitleaks 扫描命中测试位置的假值时，按值登记在仓根 `.gitleaks-test-values.toml`。

- 门禁：`bash scripts/check.sh`。`scripts/gatelib.sh`、`scripts/format.sh` 是多个仓共用的同一份（由 `gate-sync` 写入），不在本仓单独改；本仓启用哪些格式工具写在 `scripts/format.conf`。
- 改到一半只跑测试：`go test -count=1 ./...`。
- 兼容：`reading` 是导出的 Go 包（Zeta 在进程内调用它记读数），改它的导出接口要向后兼容。别的程序会调用 `record`（stdin 的三种额度写法、退出码）与 `hook session-start`（不输出、照常以 0 退出），也会直接写数据目录里的读数文件与会话账号登记（行格式见 README「数据」）。这些只能向后兼容地改。同步格式有版本号，改了要升版本，两端都要更新。
- 第三方依赖与 `go-deps-allowlist.txt` 逐行相同；要加先在清单里写明用途。
- 发版：`bash scripts/release.sh`（在 origin/master 头提交的干净检出上跑，默认升 patch，不跑门禁；加 `--gate` 先跑 `scripts/check.sh`）。Zeta 按 tag 引入本模块。
- 默认分支 `master`。
