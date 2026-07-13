package model

import "time"

// WebRole identifies the authorization boundary of a web session.
type WebRole string

const (
	// WebRoleAdmin grants full administrator access.
	WebRoleAdmin WebRole = "admin"
	// WebRoleAPIToken grants read-only access scoped to one API token.
	WebRoleAPIToken WebRole = "api_token"
)

// WebSession is a persisted browser session. TokenHash is never sent to clients.
type WebSession struct {
	TokenHash   string    `json:"-"`
	Role        WebRole   `json:"role"`
	AuthTokenID int64     `json:"auth_token_id,omitempty"`
	ExpiresAt   time.Time `json:"-"`
}
