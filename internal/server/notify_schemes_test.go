package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"cupsgolang/internal/config"
)

func TestNotifySchemesForSubscriptionsDefaultsToIppget(t *testing.T) {
	got := notifySchemesForSubscriptions(config.Config{})
	if len(got) != 1 || got[0] != "ippget" {
		t.Fatalf("notifySchemesForSubscriptions = %#v, want [ippget]", got)
	}
}

func TestNotifySchemesSupportedNormalizesExecutableName(t *testing.T) {
	base := t.TempDir()
	notifierDir := filepath.Join(base, "notifier")
	if err := os.MkdirAll(notifierDir, 0o755); err != nil {
		t.Fatalf("mkdir notifier dir: %v", err)
	}

	fileName := "mailto"
	content := []byte("#!/bin/sh\nexit 0\n")
	mode := os.FileMode(0o755)
	if runtime.GOOS == "windows" {
		fileName = "mailto.cmd"
		content = []byte("@echo off\r\nexit /b 0\r\n")
		mode = 0o644
	}
	full := filepath.Join(notifierDir, fileName)
	if err := os.WriteFile(full, content, mode); err != nil {
		t.Fatalf("write notifier: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(full, 0o755); err != nil {
			t.Fatalf("chmod notifier: %v", err)
		}
	}

	cfg := config.Config{ServerBin: base}
	schemes := notifySchemesSupported(cfg)
	if !stringInList("mailto", schemes) {
		t.Fatalf("notifySchemesSupported = %#v, want mailto", schemes)
	}

	all := notifySchemesForSubscriptions(cfg)
	if !stringInList("ippget", all) || !stringInList("mailto", all) {
		t.Fatalf("notifySchemesForSubscriptions = %#v, want ippget + mailto", all)
	}
}
