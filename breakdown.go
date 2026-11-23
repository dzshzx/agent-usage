// 周期内按模型、按角色、按会话的拆分。
package main

import (
	"sort"
	"strings"
)

// ModelRow 是周期内一个 (模型, 服务档位) 的用量。
type ModelRow struct {
	Model              *string `json:"model"`
	Tier               string  `json:"tier"`
	Requests           int64   `json:"requests"`
	InputTokens        int64   `json:"input_tokens"`
	CachedInputTokens  int64   `json:"cached_input_tokens"`
	CacheWrite5mTokens int64   `json:"cache_write_5m_tokens"`
	CacheWrite1hTokens int64   `json:"cache_write_1h_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	ReasoningTokens    int64   `json:"reasoning_tokens"`
	Cost               float64 `json:"cost"`
	UnpricedRequests   int64   `json:"unpriced_requests"`
	// 非空表示官方价格页没有该模型名，金额是借用另一模型的价格估算的
	PricedAs *string `json:"priced_as"`
}

func modelBreakdown(db *DB, book *PriceBook, key []any) ([]ModelRow, error) {
	rows, err := db.Query(
		"SELECT model, tier, COUNT(*) n, SUM(input_tokens) i, SUM(cached_input_tokens) c, "+
			"SUM(cache_write_5m_tokens) w5, SUM(cache_write_1h_tokens) w1, SUM(output_tokens) o, "+
			"SUM(reasoning_tokens) r, SUM(cost) amount, SUM(cost IS NULL) unpriced FROM "+memberSQL+" "+
			"GROUP BY model, tier ORDER BY amount DESC", key...)
	if err != nil {
		return nil, err
	}
	models := make([]ModelRow, 0, len(rows))
	for _, r := range rows {
		pricedModel, exact := book.Resolve(r.StrP("model"))
		row := ModelRow{
			Model: r.StrP("model"), Tier: r.Str("tier"), Requests: r.Int("n"), InputTokens: r.Int("i"),
			CachedInputTokens: r.Int("c"), CacheWrite5mTokens: r.Int("w5"), CacheWrite1hTokens: r.Int("w1"),
			OutputTokens: r.Int("o"), ReasoningTokens: r.Int("r"), Cost: roundTo(r.Float("amount"), 4),
			UnpricedRequests: r.Int("unpriced"),
		}
		if !exact {
			row.PricedAs = pricedModel
		}
		models = append(models, row)
	}
	return models, nil
}

// AgentRow 是 --by-agent 的一个分组：角色、模型、effort、服务档位。
type AgentRow struct {
	Agent             *string  `json:"agent"`
	Model             *string  `json:"model"`
	Effort            *string  `json:"effort"`
	Tier              string   `json:"tier"`
	Sessions          int64    `json:"sessions"`
	Requests          int64    `json:"requests"`
	InputTokens       int64    `json:"input_tokens"`
	CachedInputTokens int64    `json:"cached_input_tokens"`
	OutputTokens      int64    `json:"output_tokens"`
	Cost              float64  `json:"cost"`
	Share             *float64 `json:"share"`
}

func agentBreakdown(db *DB, key []any, total float64) ([]AgentRow, error) {
	rows, err := db.Query(
		"SELECT agent, model, effort, tier, COUNT(*) n, COUNT(DISTINCT file) sessions, SUM(input_tokens) i, "+
			"SUM(cached_input_tokens) c, SUM(output_tokens) o, SUM(cost) amount FROM "+memberSQL+" "+
			"GROUP BY agent, model, effort, tier ORDER BY amount DESC", key...)
	if err != nil {
		return nil, err
	}
	out := make([]AgentRow, 0, len(rows))
	for _, r := range rows {
		amount := r.Float("amount")
		out = append(out, AgentRow{
			Agent: r.StrP("agent"), Model: r.StrP("model"), Effort: r.StrP("effort"), Tier: r.Str("tier"),
			Sessions: r.Int("sessions"), Requests: r.Int("n"), InputTokens: r.Int("i"), CachedInputTokens: r.Int("c"),
			OutputTokens: r.Int("o"), Cost: roundTo(amount, 4), Share: share(amount, total),
		})
	}
	return out, nil
}

// share 等价于 round(amount / total, 4) if total else None。
func share(amount, total float64) *float64 {
	if total == 0 {
		return nil
	}
	return floatp(roundTo(amount/total, 4))
}

// sessionOf 返回 (会话名, 是否子代理文件)。Claude Code 的子代理日志在 <会话>/subagents/ 下，归入所在主会话；
// 其余文件（主会话日志）各自一个会话。
func sessionOf(file string) (string, bool) {
	if head, _, ok := strings.Cut(file, "/subagents/"); ok {
		return head, true
	}
	return strings.TrimSuffix(file, ".jsonl"), false
}

// SessionRow 是 --by-session 的一个会话。
type SessionRow struct {
	Session           string   `json:"session"`
	Agent             *string  `json:"agent"`
	FirstRequestAt    string   `json:"first_request_at"`
	LastRequestAt     string   `json:"last_request_at"`
	Subagents         int      `json:"subagents"`
	Requests          int64    `json:"requests"`
	InputTokens       int64    `json:"input_tokens"`
	CachedInputTokens int64    `json:"cached_input_tokens"`
	OutputTokens      int64    `json:"output_tokens"`
	Cost              float64  `json:"cost"`
	Share             *float64 `json:"share"`
	QuotaPercent      *float64 `json:"quota_percent"`
}

type sessionAcc struct {
	name      string
	agent     *string
	subagents int
	requests  int64
	first     float64
	last      float64
	i, c, o   int64
	amount    float64
}

// sessionBreakdown 按会话汇总：请求数、子代理数、token、金额、占周期金额的 share，以及按周期额度估算折成的
// quota_percent（没有估算时为 nil）。按金额降序。
func sessionBreakdown(db *DB, key []any, total float64, quota *float64) ([]SessionRow, error) {
	rows, err := db.Query(
		"SELECT file, MIN(agent) agent, COUNT(*) n, MIN(ts) first, MAX(ts) last, SUM(input_tokens) i, "+
			"SUM(cached_input_tokens) c, SUM(output_tokens) o, SUM(cost) amount FROM "+memberSQL+" GROUP BY file", key...)
	if err != nil {
		return nil, err
	}
	var order []*sessionAcc
	index := map[string]*sessionAcc{}
	for _, r := range rows {
		name, subagent := sessionOf(r.Str("file"))
		s, ok := index[name]
		if !ok {
			s = &sessionAcc{name: name, first: r.Float("first"), last: r.Float("last")}
			index[name] = s
			order = append(order, s)
		}
		if subagent {
			s.subagents++
		} else {
			s.agent = r.StrP("agent") // 会话自己的角色：主会话 main
		}
		s.requests += r.Int("n")
		s.first = minFloat(s.first, r.Float("first"))
		s.last = maxFloat(s.last, r.Float("last"))
		s.i += r.Int("i")
		s.c += r.Int("c")
		s.o += r.Int("o")
		s.amount += r.Float("amount")
	}
	sort.SliceStable(order, func(a, b int) bool { return order[a].amount > order[b].amount })
	out := make([]SessionRow, 0, len(order))
	for _, s := range order {
		row := SessionRow{
			Session: s.name, Agent: s.agent, FirstRequestAt: isoOf(s.first), LastRequestAt: isoOf(s.last),
			Subagents: s.subagents, Requests: s.requests, InputTokens: s.i, CachedInputTokens: s.c, OutputTokens: s.o,
			Cost: roundTo(s.amount, 4), Share: share(s.amount, total),
		}
		if quota != nil && *quota != 0 {
			row.QuotaPercent = floatp(roundTo(s.amount / *quota * 100, 2))
		}
		out = append(out, row)
	}
	return out, nil
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
