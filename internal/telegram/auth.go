package telegram

import (
	"log/slog"
)

// DMPolicy controls how DMs from unknown users are handled.
type DMPolicy string

const (
	DMPolicyPairing   DMPolicy = "pairing"   // unknown senders get a pairing code
	DMPolicyAllowlist DMPolicy = "allowlist"  // only config + dynamic allowlist
	DMPolicyDisabled  DMPolicy = "disabled"   // all DMs denied
)

// GroupPolicy controls how group messages are handled.
type GroupPolicy string

const (
	GroupPolicyPairing   GroupPolicy = "pairing"   // unknown group senders get a pairing code
	GroupPolicyAllowlist GroupPolicy = "allowlist"  // only allowed users in groups
	GroupPolicyDisabled  GroupPolicy = "disabled"   // all group messages denied
)

// AuthResult describes the outcome of an authorization check.
type AuthResult int

const (
	AuthAllowed     AuthResult = iota
	AuthDenied
	AuthPairing     // DM policy is pairing, user not yet approved
	AuthRateLimited
)

// SenderInfo carries Telegram user metadata for logging and pairing.
type SenderInfo struct {
	UserID    int64
	Username  string
	FirstName string
	LastName  string
	ChatID    int64
	IsGroup   bool
}

// AuthOptions configures the Auth policy engine.
type AuthOptions struct {
	DMPolicy          DMPolicy
	GroupPolicy       GroupPolicy
	ConfigUsers       []int64
	GroupAllowedUsers []int64
	AllowlistStore    *AllowlistStore
	Pairing           *PairingManager
	Limiter           *RateLimiter
}

// Auth is the policy engine for Telegram access control.
type Auth struct {
	dmPolicy          DMPolicy
	groupPolicy       GroupPolicy
	configUsers       map[int64]bool
	groupAllowedUsers map[int64]bool
	allowlistStore    *AllowlistStore
	Pairing           *PairingManager
	limiter           *RateLimiter
}

// NewAuth creates a policy-based auth engine.
func NewAuth(opts AuthOptions) *Auth {
	configUsers := make(map[int64]bool, len(opts.ConfigUsers))
	for _, id := range opts.ConfigUsers {
		configUsers[id] = true
	}
	groupUsers := make(map[int64]bool, len(opts.GroupAllowedUsers))
	for _, id := range opts.GroupAllowedUsers {
		groupUsers[id] = true
	}

	dmPolicy := opts.DMPolicy
	if dmPolicy == "" {
		dmPolicy = DMPolicyAllowlist
	}
	groupPolicy := opts.GroupPolicy
	if groupPolicy == "" {
		groupPolicy = GroupPolicyDisabled
	}

	return &Auth{
		dmPolicy:          dmPolicy,
		groupPolicy:       groupPolicy,
		configUsers:       configUsers,
		groupAllowedUsers: groupUsers,
		allowlistStore:    opts.AllowlistStore,
		Pairing:           opts.Pairing,
		limiter:           opts.Limiter,
	}
}

// Check performs the full authorization decision.
func (a *Auth) Check(sender SenderInfo) AuthResult {
	// Config allowlist is always checked first (super-admin).
	if a.configUsers[sender.UserID] {
		return AuthAllowed
	}

	// Group vs DM routing.
	if sender.IsGroup {
		return a.checkGroup(sender)
	}
	return a.checkDM(sender)
}

func (a *Auth) checkGroup(sender SenderInfo) AuthResult {
	if a.groupPolicy == GroupPolicyDisabled {
		a.logDenied(sender, "group_disabled")
		return AuthDenied
	}

	// Check group-specific allowlist.
	if a.groupAllowedUsers[sender.UserID] {
		return AuthAllowed
	}

	// Check dynamic allowlist (pairing-approved users).
	if a.inDynamicAllowlist(sender.UserID) {
		return AuthAllowed
	}

	// Rate limit check before pairing/deny response.
	if a.limiter != nil && !a.limiter.Allow(sender.UserID) {
		slog.Warn("rate limited", "user_id", sender.UserID, "username", sender.Username)
		return AuthRateLimited
	}

	if a.groupPolicy == GroupPolicyPairing {
		slog.Info("pairing required (group)",
			"user_id", sender.UserID,
			"username", sender.Username,
			"first_name", sender.FirstName,
			"chat_id", sender.ChatID,
		)
		return AuthPairing
	}

	a.logDenied(sender, "group_not_allowed")
	return AuthDenied
}

func (a *Auth) checkDM(sender SenderInfo) AuthResult {
	if a.dmPolicy == DMPolicyDisabled {
		a.logDenied(sender, "dm_disabled")
		return AuthDenied
	}

	// Check dynamic allowlist (pairing-approved users).
	if a.inDynamicAllowlist(sender.UserID) {
		return AuthAllowed
	}

	// Rate limit check before pairing/deny response.
	if a.limiter != nil && !a.limiter.Allow(sender.UserID) {
		slog.Warn("rate limited", "user_id", sender.UserID, "username", sender.Username)
		return AuthRateLimited
	}

	if a.dmPolicy == DMPolicyPairing {
		slog.Info("pairing required",
			"user_id", sender.UserID,
			"username", sender.Username,
			"first_name", sender.FirstName,
			"chat_id", sender.ChatID,
		)
		return AuthPairing
	}

	// DMPolicyAllowlist: deny if not in any allowlist.
	a.logDenied(sender, "dm_not_allowed")
	return AuthDenied
}

func (a *Auth) inDynamicAllowlist(userID int64) bool {
	if a.allowlistStore == nil {
		return false
	}
	ok, err := a.allowlistStore.Contains(userID)
	if err != nil {
		slog.Warn("allowlist store error", "error", err)
		return false
	}
	return ok
}

func (a *Auth) logDenied(sender SenderInfo, reason string) {
	slog.Warn("access denied",
		"reason", reason,
		"user_id", sender.UserID,
		"username", sender.Username,
		"first_name", sender.FirstName,
		"last_name", sender.LastName,
		"chat_id", sender.ChatID,
		"is_group", sender.IsGroup,
	)
}
