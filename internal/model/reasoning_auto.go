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
