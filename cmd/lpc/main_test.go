package main

import "testing"

func TestIsAbbrev(t *testing.T) {
	if !isAbbrev("stat", "status", 4) {
		t.Fatal("expected stat to match status")
	}
	if isAbbrev("st", "status", 4) {
		t.Fatal("expected st to fail min length")
	}
	if isAbbrev("state", "status", 4) {
		t.Fatal("expected non-prefix to fail")
	}
}

func TestParseStatusDestinations(t *testing.T) {
	got := parseStatusDestinations("Office, Lab Team")
	if len(got) != 3 || got[0] != "Office" || got[1] != "Lab" || got[2] != "Team" {
		t.Fatalf("unexpected parsed destinations: %v", got)
	}
	if got := parseStatusDestinations("all"); got != nil {
		t.Fatalf("expected nil for all, got %v", got)
	}
}

func TestDestinationMatch(t *testing.T) {
	if !destinationMatch("Office", []string{"Office", "Lab"}) {
		t.Fatal("expected exact match")
	}
	if destinationMatch("office", []string{"Office", "Lab"}) {
		t.Fatal("expected case-sensitive mismatch")
	}
}
