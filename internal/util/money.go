package util

import (
	"fmt"
	"log"
	"math"
)

const microUSDScale = 1_000_000

// USDToMicroUSD 将美元金额转换为微美元（整数），用于存储/比较以避免浮点误差。
// 对于非法输入的处理策略：
// - NaN/Inf：记录错误日志，返回 0（防御性处理）
// - 负数：记录错误日志，返回 0（费用不可能为负，这是调用方 bug）
// - 零或极小正数（四舍五入后为0）：返回 0
func USDToMicroUSD(usd float64) int64 {
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		log.Printf("[ERROR] USDToMicroUSD: invalid float value (NaN or Inf), treating as 0")
		return 0
	}
	if usd < 0 {
		log.Printf("[ERROR] USDToMicroUSD: negative USD value %v is not allowed, treating as 0", usd)
		return 0
	}
	if usd == 0 {
		return 0
	}
	return int64(math.Round(usd * microUSDScale))
}

// USDToMicroUSDSafe 将美元金额转换为微美元，返回error而不是静默处理。
// 用于需要严格验证的场景（如API输入验证）。
func USDToMicroUSDSafe(usd float64) (int64, error) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0, fmt.Errorf("invalid float value (NaN or Inf)")
	}
	if usd < 0 {
		return 0, fmt.Errorf("negative USD value %v is not allowed", usd)
	}
	if usd == 0 {
		return 0, nil
	}
	return int64(math.Round(usd * microUSDScale)), nil
}

// MicroUSDToUSD 将微美元（整数）转换为美元（浮点，仅用于展示/JSON）。
func MicroUSDToUSD(microUSD int64) float64 {
	return float64(microUSD) / microUSDScale
}
