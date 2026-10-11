package model

// ReasoningAutoConfig is the opt-in decision service configuration.
type ReasoningAutoConfig struct {
	Enabled           bool     `json:"enabled"`
	DecisionChannelID int64    `json:"decision_channel_id"`
	DecisionModel     string   `json:"decision_model"`
	BusinessModels    []string `json:"business_models"`
	TimeoutMS         int      `json:"timeout_ms"`
}

// ReasoningDecision contains decision metadata, never conversation or credentials.
type ReasoningDecision struct {
	Status            string `json:"status"`
	Trigger           string `json:"trigger,omitempty"`
	PreviousEffort    string `json:"previous_effort,omitempty"`
	SuggestedEffort   string `json:"suggested_effort,omitempty"`
	AppliedEffort     string `json:"applied_effort,omitempty"`
	Reason            string `json:"reason,omitempty"`
	DurationMS        int64  `json:"duration_ms,omitempty"`
	DecisionChannelID int64  `json:"decision_channel_id,omitempty"`
	DecisionModel     string `json:"decision_model,omitempty"`
	ContextTruncated  bool   `json:"context_truncated,omitempty"`
	ContextIncomplete bool   `json:"context_incomplete,omitempty"`
	DecisionID        string `json:"decision_id,omitempty"`
	CostUnknown       bool   `json:"cost_unknown,omitempty"`
}
