package server

import (
	"testing"

	"cupsgolang/internal/config"
	"cupsgolang/internal/model"
)

func TestUserAllowedByLimitRequireGroupFromEnvMapping(t *testing.T) {
	t.Setenv("CUPS_USER_GROUPS", "alice=printops,staff;bob=finance")
	limit := &config.LimitRule{RequireGroups: []string{"printops"}}
	if !userAllowedByLimit(model.User{Username: "alice"}, "", limit) {
		t.Fatalf("expected alice to match printops")
	}
	if userAllowedByLimit(model.User{Username: "bob"}, "", limit) {
		t.Fatalf("expected bob to not match printops")
	}
}

func TestUserAllowedByLimitSystemGroupUsesAdminFlag(t *testing.T) {
	limit := &config.LimitRule{RequireGroups: []string{"@SYSTEM"}}
	if !userAllowedByLimit(model.User{Username: "admin", IsAdmin: true}, "", limit) {
		t.Fatalf("expected admin user to satisfy @SYSTEM")
	}
	if userAllowedByLimit(model.User{Username: "user", IsAdmin: false}, "", limit) {
		t.Fatalf("expected non-admin user to fail @SYSTEM")
	}
}

func TestGroupsForUserIgnoresMalformedEntries(t *testing.T) {
	t.Setenv("CUPS_USER_GROUPS", "invalid;carol=ops qa;dave=")
	groups := groupsForUser("carol")
	if !groups["ops"] || !groups["qa"] {
		t.Fatalf("expected parsed groups for carol, got %v", groups)
	}
	if len(groupsForUser("dave")) != 0 {
		t.Fatalf("expected empty group set for dave")
	}
	if len(groupsForUser("unknown")) != 0 {
		t.Fatalf("expected empty group set for unknown user")
	}
}
