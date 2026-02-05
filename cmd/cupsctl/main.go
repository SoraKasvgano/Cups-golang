package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"cupsgolang/internal/config"
	"cupsgolang/internal/store"
)

func main() {
	args := os.Args[1:]
	cfg := config.Load()
	ctx := context.Background()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cupsctl:", err)
		os.Exit(1)
	}
	defer st.Close()

	if len(args) == 0 {
		if err := listSettings(ctx, st); err != nil {
			fmt.Fprintln(os.Stderr, "cupsctl:", err)
			os.Exit(1)
		}
		return
	}

	updates := map[string]string{}
	for _, arg := range args {
		if handled := applyFlagUpdate(updates, arg); handled {
			continue
		}
		if strings.Contains(arg, "=") {
			parts := strings.SplitN(arg, "=", 2)
			if parts[0] != "" {
				key := normalizeKey(parts[0])
				updates[key] = parts[1]
			}
		}
	}

	if len(updates) == 0 {
		if err := listSettings(ctx, st); err != nil {
			fmt.Fprintln(os.Stderr, "cupsctl:", err)
			os.Exit(1)
		}
		return
	}

	if err := applySettings(ctx, st, updates); err != nil {
		fmt.Fprintln(os.Stderr, "cupsctl:", err)
		os.Exit(1)
	}
}

func applyFlagUpdate(updates map[string]string, arg string) bool {
	switch arg {
	case "--share-printers":
		updates["_share_printers"] = "1"
		return true
	case "--no-share-printers":
		updates["_share_printers"] = "0"
		return true
	case "--debug-logging":
		updates["_debug_logging"] = "1"
		return true
	case "--no-debug-logging":
		updates["_debug_logging"] = "0"
		return true
	case "--remote-admin":
		updates["_remote_admin"] = "1"
		return true
	case "--no-remote-admin":
		updates["_remote_admin"] = "0"
		return true
	case "--remote-any":
		updates["_remote_any"] = "1"
		return true
	case "--no-remote-any":
		updates["_remote_any"] = "0"
		return true
	case "--remote-printers":
		updates["_remote_printers"] = "1"
		return true
	case "--no-remote-printers":
		updates["_remote_printers"] = "0"
		return true
	case "--user-cancel-any":
		updates["_user_cancel_any"] = "1"
		return true
	case "--no-user-cancel-any":
		updates["_user_cancel_any"] = "0"
		return true
	case "--preserve-job-history":
		updates["_preserve_job_history"] = "1"
		return true
	case "--no-preserve-job-history":
		updates["_preserve_job_history"] = "0"
		return true
	case "--preserve-job-files":
		updates["_preserve_job_files"] = "1"
		return true
	case "--no-preserve-job-files":
		updates["_preserve_job_files"] = "0"
		return true
	default:
		return false
	}
}

func normalizeKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return key
	}
	key = strings.TrimPrefix(key, "--")
	if strings.HasPrefix(key, "_") {
		return key
	}
	switch strings.ToLower(key) {
	case "share_printers":
		return "_share_printers"
	case "debug_logging":
		return "_debug_logging"
	case "remote_admin":
		return "_remote_admin"
	case "remote_any":
		return "_remote_any"
	case "remote_printers":
		return "_remote_printers"
	case "user_cancel_any":
		return "_user_cancel_any"
	case "preserve_job_history":
		return "_preserve_job_history"
	case "preserve_job_files":
		return "_preserve_job_files"
	default:
		return key
	}
}

func applySettings(ctx context.Context, st *store.Store, updates map[string]string) error {
	return st.WithTx(ctx, false, func(tx *sql.Tx) error {
		for k, v := range updates {
			if err := st.SetSetting(ctx, tx, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

func listSettings(ctx context.Context, st *store.Store) error {
	return st.WithTx(ctx, true, func(tx *sql.Tx) error {
		settings, err := st.ListSettings(ctx, tx)
		if err != nil {
			return err
		}
		if _, ok := settings["_share_printers"]; !ok {
			settings["_share_printers"] = "1"
		}
		keys := make([]string, 0, len(settings))
		for k := range settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s=%s\n", outputKey(k), settings[k])
		}
		return nil
	})
}

func outputKey(key string) string {
	switch key {
	case "_share_printers":
		return "share_printers"
	case "_debug_logging":
		return "debug_logging"
	case "_remote_admin":
		return "remote_admin"
	case "_remote_any":
		return "remote_any"
	case "_remote_printers":
		return "remote_printers"
	case "_user_cancel_any":
		return "user_cancel_any"
	case "_preserve_job_history":
		return "preserve_job_history"
	case "_preserve_job_files":
		return "preserve_job_files"
	default:
		return key
	}
}
