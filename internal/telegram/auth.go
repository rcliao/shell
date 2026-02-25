package telegram

type Auth struct {
	allowedUsers map[int64]bool
}

func NewAuth(userIDs []int64) *Auth {
	allowed := make(map[int64]bool, len(userIDs))
	for _, id := range userIDs {
		allowed[id] = true
	}
	return &Auth{allowedUsers: allowed}
}

// IsAllowed returns true if the user ID is in the allowlist.
// If the allowlist is empty, all users are allowed.
func (a *Auth) IsAllowed(userID int64) bool {
	if len(a.allowedUsers) == 0 {
		return true
	}
	return a.allowedUsers[userID]
}
