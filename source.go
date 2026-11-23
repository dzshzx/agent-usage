// 来源接口、公共属性与来源注册表；请求与价格行的数据结构。
package main

// Event 是一次模型请求（events 表的一行）。指针字段为 NULL 时是 nil。
type Event struct {
	Source             string
	Ukey               string
	Ts                 float64
	File               string
	Model              *string
	Tier               string
	InputTokens        int64
	CachedInputTokens  int64
	CacheWrite5mTokens int64
	CacheWrite1hTokens int64
	OutputTokens       int64
	ReasoningTokens    int64
	LimitID            *string
	CycleResets        *int64
	Cost               *float64
	PriceID            *int64
	Agent              *string
	Effort             *string
	Origin             *string // 从别的机器同步来的请求是那台机器的库编号；本机日志入库的为 nil
	Seq                int64   // 出处内的序号：同步来的请求带着出处给的值，本机入库时写入前现发
}

// PriceRow 是价格页解析出的一行（单位为每 1M tokens）。
type PriceRow struct {
	Model            string
	Tier             string
	Input            float64
	CachedInput      *float64
	CacheWrite       *float64
	CacheWrite1h     *float64
	Output           float64
	LongInput        *float64
	LongCachedInput  *float64
	LongCacheWrite   *float64
	LongCacheWrite1h *float64
	LongOutput       *float64
	LongThreshold    *int64
}

// priceFields 是 prices 表里的价格列，顺序与 PriceRow.values 一致。
var priceFields = []string{
	"input", "cached_input", "cache_write", "cache_write_1h", "output",
	"long_input", "long_cached_input", "long_cache_write", "long_cache_write_1h", "long_output", "long_threshold",
}

func (p PriceRow) values() []any {
	return []any{
		p.Input, nullable(p.CachedInput), nullable(p.CacheWrite), nullable(p.CacheWrite1h), p.Output,
		nullable(p.LongInput), nullable(p.LongCachedInput), nullable(p.LongCacheWrite), nullable(p.LongCacheWrite1h),
		nullable(p.LongOutput), nullableInt(p.LongThreshold),
	}
}

func nullable(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Source 是一个用量来源（Claude Code 的一个账号）。
type Source interface {
	Name() string
	Currency() string
	Symbol() string
	PricingURL() string
	// 超过长上下文阈值的请求是否按长上下文价计
	LongContextPremium() bool
	// 与另一来源共用价格表时为那个来源名：价格只按它抓取、落库、对齐与计价
	PriceFrom() string
	ParsePrices(text string) []PriceRow
	ModelKey(model string) string
	Sync(ctx *Context) (SyncStats, error)
	// 日志之外的读数来源（记录文件、接口）在这里入库
	CollectReadings(ctx *Context) error
}

// SyncStats 是一次日志同步的统计。
type SyncStats struct {
	FilesScanned int `json:"files_scanned"`
	FilesRead    int `json:"files_read"`
	EventsAdded  int `json:"events_added"`
}

// baseSource 提供各来源共用的默认属性。
type baseSource struct {
	name               string
	currency           string
	symbol             string
	pricingURL         string
	longContextPremium bool
	priceFrom          string
}

func (b *baseSource) Name() string                 { return b.name }
func (b *baseSource) Currency() string             { return b.currency }
func (b *baseSource) Symbol() string               { return b.symbol }
func (b *baseSource) PricingURL() string           { return b.pricingURL }
func (b *baseSource) LongContextPremium() bool     { return b.longContextPremium }
func (b *baseSource) PriceFrom() string            { return b.priceFrom }
func (b *baseSource) ModelKey(model string) string { return model }

// priceOwner 返回负责价格表的来源；价格相关的读写一律经它。
func priceOwner(src Source) Source {
	if from := src.PriceFrom(); from != "" {
		return sources[from]
	}
	return src
}

// sources 是来源名 → 来源实例，各来源在运行时按名互查。
var sources = map[string]Source{}

// sourceNames 按名排序的全部来源名。
func sourceNames() []string {
	names := make([]string, 0, len(sources))
	for n := range sources {
		names = append(names, n)
	}
	sortStrings(names)
	return names
}

func init() { resetSources() }

// resetSources 把账号表清回只有主号；各账号随后由 discoverAccounts 与入库重新登记。
func resetSources() {
	clear(sources)
	sources[primaryAccount] = primary
}
