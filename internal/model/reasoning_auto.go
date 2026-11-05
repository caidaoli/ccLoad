package model

/**
 * @brief 按模型路由的动态思考判断配置
 */
type ReasoningAutoConfig struct {
	Enabled        bool     `json:"enabled"`         ///< 是否启用动态判断
	DecisionModel  string   `json:"decision_model"`  ///< System One 逻辑模型名
	BusinessModels []string `json:"business_models"` ///< 生效业务模型的精确基名
	TimeoutMS      int      `json:"timeout_ms"`      ///< 整次判断的总超时，单位毫秒
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
