package server

import (
	"os"
	"strings"

	"cupsgolang/internal/config"
	"cupsgolang/internal/model"
)

func userAllowedByLimit(u model.User, owner string, limit *config.LimitRule) bool {
	if limit == nil {
		return true
	}
	username := strings.TrimSpace(u.Username)
	if username == "" {
		username = "anonymous"
	}

	// "Require valid-user" / "Require user" (no selectors) means any authenticated
	// user is allowed.
	if limit.RequireUser && !limit.RequireOwner && !limit.RequireAdmin && len(limit.RequireUsers) == 0 && len(limit.RequireGroups) == 0 {
		return true
	}

	// Explicit allowed users.
	for _, ru := range limit.RequireUsers {
		if strings.EqualFold(strings.TrimSpace(ru), username) {
			return true
		}
	}

	// Job owner.
	if limit.RequireOwner && strings.TrimSpace(owner) != "" &&
		strings.EqualFold(username, strings.TrimSpace(owner)) {
		return true
	}

	// Group membership. CUPS checks system groups; we support admin/system tokens
	// plus configurable user->group mappings via CUPS_USER_GROUPS.
	if len(limit.RequireGroups) > 0 && userMatchesGroups(username, u.IsAdmin, limit.RequireGroups) {
		return true
	}

	// Admin user.
	if limit.RequireAdmin && u.IsAdmin {
		return true
	}

	return false
}

func userAllowedByLocation(u model.User, rule *config.LocationRule) bool {
	if rule == nil {
		return true
	}
	username := strings.TrimSpace(u.Username)
	if username == "" {
		username = "anonymous"
	}

	if rule.RequireUser && !rule.RequireAdmin && len(rule.RequireUsers) == 0 && len(rule.RequireGroups) == 0 {
		return true
	}
	for _, ru := range rule.RequireUsers {
		if strings.EqualFold(strings.TrimSpace(ru), username) {
			return true
		}
	}
	if len(rule.RequireGroups) > 0 && userMatchesGroups(username, u.IsAdmin, rule.RequireGroups) {
		return true
	}
	if rule.RequireAdmin && u.IsAdmin {
		return true
	}
	return false
}

func hasAdminGroupToken(groups []string) bool {
	for _, g := range groups {
		switch normalizeGroupToken(g) {
		case "system", "admin", "lpadmin":
			return true
		}
	}
	return false
}

func userMatchesGroups(username string, isAdmin bool, required []string) bool {
	if len(required) == 0 {
		return true
	}
	if isAdmin && hasAdminGroupToken(required) {
		return true
	}
	groups := groupsForUser(username)
	if len(groups) == 0 {
		return false
	}
	for _, need := range required {
		n := normalizeGroupToken(need)
		if n == "" {
			continue
		}
		if groups[n] {
			return true
		}
	}
	return false
}

func groupsForUser(username string) map[string]bool {
	out := map[string]bool{}
	username = strings.TrimSpace(strings.ToLower(username))
	if username == "" {
		return out
	}
	env := strings.TrimSpace(os.Getenv("CUPS_USER_GROUPS"))
	if env == "" {
		return out
	}
	entries := strings.FieldsFunc(env, func(r rune) bool {
		return r == ';' || r == '\n' || r == '\r'
	})
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}
		userPart := strings.TrimSpace(strings.ToLower(parts[0]))
		if userPart != username {
			continue
		}
		for _, g := range strings.FieldsFunc(parts[1], func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t'
		}) {
			n := normalizeGroupToken(g)
			if n != "" {
				out[n] = true
			}
		}
	}
	return out
}

func normalizeGroupToken(group string) string {
	g := strings.TrimSpace(strings.ToLower(group))
	g = strings.TrimPrefix(g, "@")
	return g
}
