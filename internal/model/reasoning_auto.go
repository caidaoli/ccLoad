package model

/**
 * @brief 按模型路由的动态思考判断配置
 */
type ReasoningAutoConfig struct {
	Enabled        bool             `json:"enabled"`                   ///< 是否启用动态判断
	DecisionModel  string           `json:"decision_model"`            ///< 决策渠道的逻辑模型名
	BusinessModels []string         `json:"business_models"`           ///< 生效业务模型的精确基名
	TimeoutMS      int              `json:"timeout_ms"`                ///< 整次判断的总超时，单位毫秒
	PromptTemplate string           `json:"prompt_template,omitempty"` ///< 空值或 auto 随实际判断模型选择内置模板
	CustomPrompt   *ReasoningPrompt `json:"custom_prompt,omitempty"`   ///< 自定义判断指令快照，仅 custom 模式使用
}

/**
 * @brief 可编辑的判断指令及完整五档标准
 */
type ReasoningPrompt struct {
	Instructions string                `json:"instructions"`    ///< 主判断指令
	Criteria     map[string]string     `json:"criteria"`        ///< low 至 max 的五档描述
	Depth        *ReasoningDepthPrompt `json:"depth,omitempty"` ///< 存在时采用 Jev 两问策略，否则采用单题 score
}

/**
 * @brief 困难任务的深度判断指令，主判断为 high 时采用
 */
type ReasoningDepthPrompt struct {
	Instructions string            `json:"instructions"` ///< 深度判断指令
	Criteria     map[string]string `json:"criteria"`     ///< high、xhigh、max 三档描述
}

// ReasoningDecision contains decision metadata, never conversation or credentials.
type ReasoningDecision struct {
	Status  string `json:"status"`
	Trigger string `json:"trigger,omitempty"`
	// ClientEffort is the level the client asked for, before any automatic change.
	ClientEffort    string `json:"client_effort,omitempty"`
	PreviousEffort  string `json:"previous_effort,omitempty"`
	SuggestedEffort string `json:"suggested_effort,omitempty"`
	// Confidence is the decision model's own score when it reports one.
	Confidence        *float64 `json:"confidence,omitempty"`
	AppliedEffort     string   `json:"applied_effort,omitempty"`
	Reason            string   `json:"reason,omitempty"`
	DurationMS        int64    `json:"duration_ms,omitempty"`
	ContextIncomplete bool     `json:"context_incomplete,omitempty"`
	CostUnknown       bool     `json:"cost_unknown,omitempty"`
}
