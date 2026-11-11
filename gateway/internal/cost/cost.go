// 本包按 Provider usage 计算人民币成本，并统一到 0.0001 元精度。
package cost

import (
	"fmt"
	"math"

	"github.com/carder-coder/ai-agent/gateway/internal/provider"
)

// Prices 为每百万 token 的输入、缓存输入和输出价格。
type Prices struct {
	Input  float64
	Output float64
	Cached float64
}

// Calculate 根据 usage 分别计费，并将结果四舍五入到万分之一元。
func Calculate(usage provider.Usage, prices Prices) (float64, error) {
	rawCost, err := rawCost(usage, prices)
	if err != nil {
		return 0, err
	}
	// 以整数万分之一元舍入，避免把格式化显示精度误当成计费精度。
	return math.Round(rawCost*10_000) / 10_000, nil
}

// UpperBound 将估算成本向上取整到 0.0001 元，避免预算预留因四舍五入不足。
func UpperBound(usage provider.Usage, prices Prices) (float64, error) {
	rawCost, err := rawCost(usage, prices)
	if err != nil {
		return 0, err
	}
	return math.Ceil(rawCost*10_000) / 10_000, nil
}

func rawCost(usage provider.Usage, prices Prices) (float64, error) {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.TotalTokens < 0 ||
		usage.CachedTokens < 0 || usage.CachedTokens > usage.InputTokens {
		return 0, fmt.Errorf("invalid token usage: %+v", usage)
	}
	if prices.Input < 0 || prices.Output < 0 || prices.Cached < 0 ||
		math.IsNaN(prices.Input) || math.IsNaN(prices.Output) || math.IsNaN(prices.Cached) ||
		math.IsInf(prices.Input, 0) || math.IsInf(prices.Output, 0) || math.IsInf(prices.Cached, 0) {
		return 0, fmt.Errorf("token prices must be non-negative")
	}

	regularInput := usage.InputTokens - usage.CachedTokens
	rawCost := (float64(regularInput)*prices.Input +
		float64(usage.CachedTokens)*prices.Cached +
		float64(usage.OutputTokens)*prices.Output) / 1_000_000
	return rawCost, nil
}
