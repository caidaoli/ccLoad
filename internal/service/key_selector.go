package service

import "ccLoad/internal/model"

// KeySelector 接口定义了Key选择策略
// 阶段 7：提取接口以解决循环依赖（app ↔ service）
//
// 设计原因：
// - ProxyService需要使用KeySelector来选择可用的API Key
// - 但KeySelector的具体实现在app包中
// - 通过定义接口，ProxyService依赖抽象而不是具体实现（DIP原则）
//
// 实现：internal/app/KeySelector
type KeySelector interface {
	// SelectAvailableKey 从给定的API Keys列表中选择一个可用的Key
	//
	// 参数：
	//   - channelID: 渠道ID（用于轮询策略的计数器）
	//   - apiKeys: 可选的API Keys列表
	//   - excludeKeys: 本次请求中已尝试过的Key索引（用于重试时排除）
	//
	// 返回：
	//   - keyIndex: 选中的Key索引（0-based）
	//   - apiKey: 选中的Key字符串
	//   - error: 如果没有可用Key或所有Key都在冷却中
	//
	// 策略：
	//   - 单Key场景：直接返回唯一Key，不使用Key级别冷却（YAGNI原则）
	//   - 多Key场景：根据KeyStrategy选择
	//     - "sequential": 顺序访问（优先使用第一个可用Key）
	//     - "round_robin": 轮询访问（均匀分布负载）
	SelectAvailableKey(channelID int64, apiKeys []*model.APIKey, excludeKeys map[int]bool) (int, string, error)
}
