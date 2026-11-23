# agent-usage

统计 [Claude Code](https://docs.anthropic.com/en/docs/claude-code) 各 claude.ai 账号在 7 天限额周期内各模型的 token 用量，按 Anthropic 官方 API 价格折算成美元，再由周期已用百分比反推这个周期的总额度：订阅一周大概值多少 API 钱、还剩多少。几台 Linux 机器用同一批账号时，`sync` 把各机器的用量合到一起。另有 `time` 子命令，看主会话的时间花在哪。默认输出 JSON，全程无交互。

非官方工具，与 Anthropic 无关，见文末「与 Claude Code 的关系」。

[English](#english)

## 它怎么算

- **用量与金额**：读 `~/.claude/projects/**/*.jsonl`（Claude Code 的会话日志，含子代理），每条请求按请求当时的官方价格算出金额，存进本地 SQLite 库。
- **已用百分比（读数）**：Claude Code 不把额度写进文件，只交给它的调用方。跑着 Claude Code 的程序每拿到一份额度，就交给 `agent-usage record` 记下（见「接入」）。
- **总额度估算**：周期累计金额 ÷ 已用百分比，读数够多时改用两次读数之间的金额增量 ÷ 百分比增量，见「输出」里的 `quota_estimate`。
- **多个账号**：几个 claude.ai 账号用 [claude-switch](https://github.com/dzshzx/claude-switch) 冷切换时共用同一份会话日志。`agent-usage hook session-start` 在每次会话启动时登记它属于哪个号，据此把请求分回各账号；没有登记的会话都算主号 `claude`。

## 安装

要 Go（版本见 `go.mod`）：

```sh
go install github.com/dzshzx/agent-usage@latest
```

程序装在 `$(go env GOPATH)/bin`。SQLite 是纯 Go 实现，不需要 cgo，可以直接交叉编译给别的机器（比如 `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build`）。

## 接入

### 额度读数：`agent-usage record`

Claude Code 在两处交出额度：

- 用 `--output-format stream-json` 跑时，每次响应后输出一行 `{"type":"rate_limit_event","rate_limit_info":{…}}`，`rate_limit_info.unifiedWindows.seven_day` 是 7 天窗口：`utilization` 是 0～1 的比例，`resetsAt` 是重置时刻（epoch 秒）；
- 经 stream-json 的 control 请求 `get_usage` 问到的应答：`rate_limits.seven_day` 的 `utilization` 是 0～100 的百分比，`resets_at` 是 ISO-8601 时间。

调用方把其中任何一种（整行、只有 `rate_limit_info`、或 `get_usage` 应答的那个对象）从 stdin 交给 `agent-usage record`：

```sh
claude -p --output-format stream-json --verbose "…" | jq -c 'select(.type == "rate_limit_event")' |
  while read -r line; do printf '%s\n' "$line" | agent-usage record; done
```

读数记在当前账号名下：按 claude-switch 的切换记录（没装 claude-switch 时就是 `claude`），`--account` 可以指定。当前是 claude-switch 的非账号目标（不登录 claude.ai，比如经 API 网关）时没有额度，什么都不记。只在读数有变化时追加一行：重置时间更晚（新周期），或同一周期里百分比更高；同一周期里比上一条低 3 个点以上时记一条回落（官方中途清零、重算档位），估算在那里切段。几个程序同时调用也不会写乱。stdin 不是这几种写法时退出码 1；认得但没带 7 天窗口时什么都不记，退出码 0。

[Zeta](https://github.com/dzshzx/Zeta) 的主机端会自动调用它。

### 账号登记：`agent-usage hook session-start`

只用一个账号时不用装。用 claude-switch 切换几个账号时，在 `~/.claude/settings.json` 里加一个 SessionStart hook：

```json
{
  "hooks": {
    "SessionStart": [{ "hooks": [{ "type": "command", "command": "agent-usage hook session-start" }] }]
  }
}
```

（`command` 里写 agent-usage 的完整路径更稳，hook 运行时的 PATH 不一定含 `$(go env GOPATH)/bin`。）

它在每次会话启动（startup、resume、clear、compact）向登记文件追加一行，从不输出（SessionStart 的输出会进会话上下文），出错只写 stderr，不影响会话。归属规则：

- 每条请求按日志里的 `sessionId`（子代理日志里也是主会话的 id）取该会话在请求时刻及之前的最后一条登记；没有登记的会话归 `claude`。已入库的请求不会回溯改归属。
- 同一会话 resume 时换了号，切换前后的请求按时间分到两边。resume 会把旧消息复制进新会话文件，复制件带新会话的 id 和原来的时间戳；这类消息等本轮所有文件扫完后再定归属：原件已入库就沿用原件的号，各号之间只计一次，查不到原件才归 `claude`。结果与文件扫描顺序无关。
- 非账号目标那几段会话的请求照常入库，但不进任何报告、也不同步给别的机器，只用来让 resume 复制件认出原件。
- 会话日志只扫描一遍；各账号共用主号的价格表。

### 定时同步（可选）

有 systemd 的机器可以用 `contrib/systemd/` 里的用户级单元每 3 小时跑一次 `agent-usage sync`：

```sh
cp contrib/systemd/agent-usage-sync.{service,timer} ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now agent-usage-sync.timer
```

单元里的程序路径是 `%h/go/bin/agent-usage`，装在别处时改 `ExecStart`。

## 用法

```sh
agent-usage status --text      # 进行中的周期
agent-usage history --text     # 全部周期
agent-usage daily --text       # 按日趋势
agent-usage status --by-session --text   # 本周期按会话看花费
```

子命令、选项、环境变量与退出码见 `agent-usage --help`。首次运行要把全部会话日志入库，几 GB 日志约一分钟，之后每次约 2 秒。入库发生在每次调用时；Claude Code 默认 30 天清理会话日志，两次调用的间隔要短于 30 天，其间的用量才能完整入库。

报表列出的账号不写死：主号 `claude` 总在；数据目录里每个 `rate-limits.<账号>.jsonl` 各算一个；库里有 7 天窗口请求或读数的账号（会话日志里新出现的、从别的机器同步来的）也算。`--source` 可以点名任何合账号名写法的名字，没有数据的账号输出空块。

## 输出

顶层 `sources.<来源>`（按来源名排序，`--text` 同序分段）各自带 `currency`、`sync`、`prices`，以及按命令不同的 `cycles`、`daily` 或 `prices_table`。`daily[]` 每项是一天，`models` 是模型 → 当天金额，按金额从高到低列出。

`cycles[]` 每项是一个限额周期：

| 字段                                      | 含义                                                                                                                                                                                                                                                                                                                                                                                          |
| ----------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `limit_id`、`window_minutes`、`plan`      | 限额名、窗口长度、订阅档位                                                                                                                                                                                                                                                                                                                                                                    |
| `active`、`window_starts_at`、`resets_at` | 是否进行中、名义起止时间。官方提前重置限额时，旧周期在 `last_request_at` 附近实际结束                                                                                                                                                                                                                                                                                                         |
| `inferred`                                | 为真表示这段时间没有任何读数，周期边界是按相邻已知重置时间前后推 7 天得出的，不给额度估算                                                                                                                                                                                                                                                                                                     |
| `used_percent`、`used_percent_at`         | 最新已用百分比及其读数时刻                                                                                                                                                                                                                                                                                                                                                                    |
| `cost`、`requests`                        | 该周期内全部请求按官方价折算的金额与请求数                                                                                                                                                                                                                                                                                                                                                    |
| `models[]`                                | 按模型与服务档位分组的请求数、各类 token 数与金额；`input_tokens` 含缓存命中与缓存写入部分，`output_tokens` 含推理部分                                                                                                                                                                                                                                                                        |
| `models[].priced_as`                      | 非空表示官方价格页没有该模型名，金额借用所列模型的价格估算                                                                                                                                                                                                                                                                                                                                    |
| `unpriced_requests`                       | 价格表里找不到模型、未计入金额的请求数                                                                                                                                                                                                                                                                                                                                                        |
| `quota_estimate.estimate`                 | 周期总额度估算，取值方法见 `method`，`readings` 是所用读数个数                                                                                                                                                                                                                                                                                                                                |
| `drops[]`                                 | 周期内的读数回落（官方中途清零、重算档位），每次一项：`at` 是回落后第一条读数的时刻，`from`、`to` 是回落前后的百分比；没有回落时不出现                                                                                                                                                                                                                                                        |
| `agents[]`                                | 仅 `--by-agent`：按 `agent`、`model`、`effort`、`tier` 分组的会话数、请求数、token 数、金额与占周期金额的 `share`                                                                                                                                                                                                                                                                             |
| `sessions[]`                              | 仅 `--by-session`：按会话分组、按金额降序。`session` 是会话日志路径去掉 `.jsonl`（子代理日志在 `<会话>/subagents/` 下，归入所在主会话）；`agent` 是该会话自己的角色（主会话 `main`）；另给首末请求时刻、`subagents`（归入的子代理文件数）、请求数、token 数、金额、占周期金额的 `share`，以及 `quota_percent`：金额按该周期的 `quota_estimate.estimate` 折成的额度百分比，没有估算时为 `null` |

`segments[]`（仅 `segments` 命令）每项是 `[start, end)` 内该来源的全部请求。时间参数按本地时区读 `YYYY-MM-DD`、`YYYY-MM-DD HH:MM[:SS]`（中间也可用 `T`），也认带时区的 RFC 3339；参数不合法时退出码 2。

| 字段                                                                            | 含义                                                                                                         |
| ------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| `cost`、`requests`、`unpriced_requests`、`cost_per_request`                     | 金额、请求数、未定价请求数、平均每次请求的金额                                                               |
| `active_hours`、`cost_per_active_hour`、`requests_per_active_hour`              | 请求数不少于 20 的自然小时数，以及这些小时内平均每小时的金额与请求数；零星几次请求的小时不计，免得摊薄       |
| `avg_context_tokens`、`cache_hit_ratio`                                         | 每次请求的平均输入 token（含缓存读写，即上下文大小）与其中读缓存的比例                                       |
| `cost_parts`                                                                    | 金额按新输入、读缓存、写缓存、输出拆开，按请求当时的价格版本重算，未定价请求不计                             |
| `context_buckets[]`                                                             | 按每次请求的上下文大小分档（100K 一档，500K 以上一档）的请求数、金额与各自占比                               |
| `sessions`、`subagents`、`subagent_cost`、`subagent_share`、`cost_per_subagent` | 涉及的主会话数、子代理日志文件数、子代理请求的金额、占比与每个子代理的平均金额                               |
| `agents[]`、`efforts[]`                                                         | 按角色、按 effort 分组的文件数、请求数、金额、占比与每次请求的平均输出 token；取不到值的一组 `key` 为 `null` |

`--text` 下第二段起每段末尾另起一行，列出相对上一段每次请求金额、每活跃小时金额、平均上下文的变化与子代理占比。

### time

`time` 直接读 `~/.claude/projects` 下的会话日志，不开数据库、不同步、不对齐价格，也不分账号：顶层 `sources` 为空，结果在顶层 `time[]`，每项是 `[start, end)` 一段，切段参数与 `segments` 相同。它只覆盖日志还在的时段（Claude Code 默认 30 天清理），本机两周约 3.5 GB 日志扫一遍约 4 秒。

主会话的时间按「一轮」算：从提问到这一轮收工，取日志里的 `turn_duration` 记录，中间等后台任务或子代理的时间也在内；同一会话的各轮去重叠后相加。一轮内的每一刻归到下面一类，重叠时取靠前的：等用户、压缩、跑命令、模型、等后台与消息，都不是的归其他。

| 字段                                                                 | 含义                                                                                                                                                                                                                                                                                        |
| -------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `sessions`、`task_hours`                                             | 段内有活动的主会话数，以及各轮去重叠后的合计小时                                                                                                                                                                                                                                            |
| `long_stretch_hours`                                                 | 等用户、等后台、其他里单段连续超过 30 分钟的小时数，多半是过夜；单列，不计入 `parts`                                                                                                                                                                                                        |
| `parts[]`                                                            | 按 `model`（模型在生成）、`tools`（前台工具在跑）、`waiting_background`（上一个动作到后台任务、子代理或别的会话来通知之间）、`waiting_user`（提问弹窗、计划确认、用户手动批准的命令）、`compaction`、`other` 给出小时数与占比，都已剔除长等待                                               |
| `model_calls`、`model_call_median_seconds`、`model_call_p90_seconds` | 主会话在段内发出的模型请求数，以及单次请求从发出到最后一块内容写完的秒数                                                                                                                                                                                                                    |
| `advisor`                                                            | 含 advisor 的请求次数、合计小时与占模型请求时间的比例；advisor 的等待算在这次请求里                                                                                                                                                                                                         |
| `models[]`                                                           | 按模型与档位分组的请求数、合计小时与单次中位秒数，只列耗时最多的 6 组                                                                                                                                                                                                                       |
| `bash[]`                                                             | 主会话前台 Bash 调用按用途分组的次数、小时与占比：`merge_release`（`git push`、`gh pr merge`、`gh release create`、`wt merge`、`wt land`）、`wait_poll`（`sleep`、`until` 等轮询）、`gate_test`（门禁脚本与测试、构建）、`remote`（`ssh`、`scp`、`rsync`）、`other`；从前往后取第一个匹配的 |
| `subagents`                                                          | 子代理日志的文件数、请求数、模型请求小时、前台工具小时与 advisor；各子代理并行，小时数是相加的，不是实际用时                                                                                                                                                                                |

请求与工具调用按开始时刻归段；一轮跨过切点时按切点裁开。模型请求的起点取日志里的 `api-request` 记录，没有这类记录的旧日志算不出模型时间，落在 `other`。后台启动的命令和子代理（调用本身立刻返回）不计入 `tools`。同一条请求或工具调用被复制进多个文件时（resume、子代理续接）按 id 只计一次，留路径靠前的那份。一段长等待跨过切点时按切点裁成两截，各自再按 30 分钟判断。

`agent` 为 `main` 表示主会话，否则是子代理的角色名，取子代理日志旁 `agent-<id>.meta.json` 的 `agentType`。`effort` 按每次请求当时生效的值记，取每条消息的 `effort`。取不到时为 `null`，文本输出显示为 `?` 或 `-`。

`quota_estimate` 给两种口径。`ratio` 是最新读数时刻的周期累计金额除以已用百分比；读数是整数时附 `ratio_range`，即取整带来的区间。`delta` 用百分比两次上跳之间的金额增量计算，读数跨度满 3 个百分点才给出，给出时 `estimate` 采用它。周期内有回落时，读数在回落处切成几段：`ratio` 取各段金额增量之和除以各段百分比增量之和（第一段从周期起点算，之后各段从回落那条读数算），`delta` 的上跳只在段内配对，横跨回落的那段金额两边都不计，也不给 `ratio_range`。

`quota_reference.median` 是最近 8 个已用不低于 30% 的周期的估算中位数，比单个周期的估算稳定。订阅额度按 Anthropic 内部口径计量，与 API 价并非严格成比例，不同模型配比的周期之间估算值会有差异。

## 多台机器同步

每台机器的库只从本机的会话日志入库。同一个号在几台机器上用时，读数（已用百分比）是账号全局的，金额却只含本机的请求，额度估算就偏低。`sync` 把对端库里各账号的请求与读数合并进本机，也把本机的推给对端：

- 一次连接：每个对端只开一次 ssh，对端只启动一次 agent-usage（`peer`）。本机先报自己有什么，对端回它的情况和本机缺的行，本机收下后再发对端缺的行。没有新用量时整次同步不到 1 秒。
- 只传对方缺的：每行只由最初入库的那台机器（出处，`origin`）产生和修改，每台机器给自己入库的行按序编号（请求新增或补全时换新号）。每个库记着各出处已经见过的最大序号（版本向量）。同步时双方先交换向量，各自只发对方向量之后的行；删库重建的一端向量为空，自然拿到全量。
- 累计：发给对方的也包括从别的机器同步来的行。A 与 B、B 与 C 各自同步后，三台都有全部用量。请求按消息 id 合并：同一消息只记一行，已记在别的账号下时不再记。金额按收的一端自己的价格表重算；读数按值去重。
- 体积：每行是 JSON 数组，重复的字符串只在第一次出现时写全文，之后写编号；时间与序号写与上一行之差；分帧用 zstd 压缩。几十万条请求的整份数据约 20 MB，增量通常几 KB。格式见 `transfer.go` 开头。
- 账号对应：两边按账号名对应。两边的 claude-switch 都记了同一个名字、账号编号却不同时，整份拒绝导入。库是从另一台拷贝来的（两边库编号相同）时也拒绝，报错里给出换编号的命令。
- 传输：`ssh <主机> ~/.local/bin/agent-usage peer`（`BatchMode`，连接超时 5 秒），对端要装好同一版 agent-usage（同步格式有版本号，不同时拒绝导入）；路径不同时用 `--remote-command` 或 `AGENT_USAGE_REMOTE_COMMAND`。`export` 与 `import` 也能单独用：`agent-usage export > 文件` 写整份，在另一台 `agent-usage import < 文件` 合并。

三种触发方式：

| 方式 | 做法                                                                                                                                                                                                                |
| ---- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 手动 | `agent-usage sync [主机…]`，不给主机时同步配置里的全部对端                                                                                                                                                          |
| 自动 | `status`、`history`、`daily`、`segments` 运行时，距上次与某个对端尝试同步（成败都算）超过 `auto_sync_minutes` 才顺带同步它，对端离线时每个间隔最多等一次连接超时；`--offline`、`--no-sync` 时不同步，连不上只记警告 |
| 定时 | 见「接入」里的定时同步                                                                                                                                                                                              |

配置在 `~/.config/agent-usage/config.json`（`--config`、`AGENT_USAGE_CONFIG`），没有这个文件时没有对端、也不自动同步：

```json
{ "peers": ["phone"], "auto_sync_minutes": 60 }
```

`peers` 是 ssh 主机名。`auto_sync_minutes` 不写时是 60，写 0 关闭自动同步。`sync` 的输出每台对端给拉回（`pulled`）、推送（`pushed`）各一份新增条数；对端连不上（ssh 退出码 255）只记警告、退出码 0，对端的 agent-usage 报错时退出码 1。只用一台机器的不用配。

## 数据

数据目录是 `~/.local/share/agent-usage`（`--data-dir`、`AGENT_USAGE_DATA_DIR`）：

| 文件                       | 内容                                                                                                                                                          |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `usage.db`                 | SQLite 库，可直接查询做长期统计；表结构见 `common.go` 的 `schema`、`eventsTable` 与 `eventIndexes`                                                            |
| `rate-limits.jsonl`        | 主号的读数，每行 `{"ts": 1789975427, "window": "seven_day", "used_percentage": 31, "resets_at": 1790442000}`，回落那行另带 `"dropped_from": <回落前的百分比>` |
| `rate-limits.<账号>.jsonl` | 其他账号的读数，行格式相同；旁边的 `.state` 是上一条记下的读数，`rate-limits.lock` 是并发写入的锁                                                             |
| `session-accounts.jsonl`   | 会话账号登记，每行 `{"ts": 1792200000.5, "session_id": "<会话 id>", "account": "claude"}`，非账号目标另带 `"target": true`；不合账号名写法的行忽略            |

读数文件与登记文件是追加写的文本，入库时按行读，可以自己写入（比如从别的来源补读数），按上面的格式即可。

库里 `meta` 表存本库编号与和各对端上次尝试同步的时刻；`events.origin`、`readings.origin` 是从别的机器同步来的行所出自的库编号（本机入库的为空），`seq` 是行在出处内的序号；`origins` 表存各外来出处已见过的最大序号。

删掉数据库后，用量会从会话日志重新入库，别的机器的用量可以再同步回来；价格变更历史、Claude Code 已清理掉的会话日志里的用量，以及只在这台机器上、对端也没有的读数无法重建。历史请求的金额按首次采集到的价格估算。

## 测试

```sh
go test ./...       # 门禁：bash scripts/check.sh
```

`testdata/` 里是 Anthropic 官方价格页样本与 `--text` 的输出样本。价格页改版、解析器需要调整时，先用新页面替换对应样本，再改解析器和断言。有意改动 `--text` 时用 `UPDATE_GOLDEN=1 go test -run TestRenderTextMatchesSavedSample .` 重录输出样本，提交前看 diff。

## 价格流程

金额在请求入库时按已采集价格算好并落库；对齐周期、重试与价格版本规则见 `prices.go`，价格页地址见 `claude.go`。

- 价格页用展示名（`Claude Opus 4.1`），日志用 API id（`claude-opus-4-1-20250805`），两边归一成「家族名 + 版本数字」（`opus41`）后比对，对不上的计入 `unpriced_requests`。
- 按提示长度分档的模型（Claude Haiku 5.5）在价格表里占「up to 100,000 tokens」「over 100,000 tokens」两行，并成一条：前一行是基础价，后一行是长上下文价；一次请求的全部输入（含读缓存与写缓存）超过阈值时，整次请求按长上下文价计。
- fast mode 表的缓存价按官方说明沿用标准档倍率。

## 与 Claude Code 的关系

这是个人写的非官方工具，与 Anthropic 没有关系，也没有得到 Anthropic 的认可。Claude 与 Claude Code 是 Anthropic 的产品与商标。

agent-usage 不修改 Claude Code，不调用 Anthropic 的接口，只读 Claude Code 在本机写的会话日志、Claude Code 交给调用方的额度，以及 Anthropic 公开的价格页。折算出的金额是按 API 价格的估算，订阅额度按 Anthropic 内部口径计量，与 API 价并非严格成比例。Claude Code 改了会话日志或额度的格式时，agent-usage 可能要跟着改。

## 许可证

[MIT](LICENSE)

---

## English

agent-usage reports, per claude.ai account, the token usage of [Claude Code](https://docs.anthropic.com/en/docs/claude-code) in each 7-day limit cycle, prices it at Anthropic's public API rates (USD), and estimates the cycle's total quota from the used percentage. It is an unofficial tool, not affiliated with or endorsed by Anthropic.

**Install.** `go install github.com/dzshzx/agent-usage@latest` (pure-Go SQLite, no cgo).

**Inputs.** Usage comes from Claude Code's session logs (`~/.claude/projects/**/*.jsonl`). Claude Code does not write its quota to disk; programs that run it pipe each `rate_limit_event` line of `--output-format stream-json` output (or the `get_usage` control response) to `agent-usage record`, which appends a reading for the current account when it changes. When several accounts share one machine via [claude-switch](https://github.com/dzshzx/claude-switch), add `agent-usage hook session-start` as a SessionStart hook so each session is attributed to its account; sessions on a claude-switch settings target are stored but never reported.

**Usage.** `agent-usage status --text` (current cycle), `history`, `daily`, `prices`, `segments`, `time`; `sync [host…]` merges usage with other machines over ssh. JSON by default. Data lives in `~/.local/share/agent-usage`. See `agent-usage --help`.

**License.** [MIT](LICENSE)
