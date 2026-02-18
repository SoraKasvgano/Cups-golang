package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"cupsgolang/internal/config"
	"cupsgolang/internal/store"
)

var errShowHelp = errors.New("show-help")

type options struct {
	server  string
	encrypt bool
	user    string
	updates map[string]string
}

var disallowedDirectives = []string{
	"AccessLog",
	"CacheDir",
	"ConfigFilePerm",
	"DataDir",
	"DocumentRoot",
	"ErrorLog",
	"FatalErrors",
	"FileDevice",
	"Group",
	"Listen",
	"LogFilePerm",
	"PageLog",
	"PassEnv",
	"Port",
	"Printcap",
	"PrintcapFormat",
	"RemoteRoot",
	"RequestRoot",
	"ServerBin",
	"ServerCertificate",
	"ServerKey",
	"ServerKeychain",
	"ServerRoot",
	"SetEnv",
	"StateDir",
	"SystemGroup",
	"SystemGroupAuthKey",
	"TempDir",
	"User",
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if errors.Is(err, errShowHelp) {
		usage("")
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cupsctl:", err)
		os.Exit(1)
	}

	cfg := config.Load()
	ctx := context.Background()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cupsctl:", err)
		os.Exit(1)
	}
	defer st.Close()

	if len(opts.updates) == 0 {
		if err := listSettings(ctx, st); err != nil {
			fmt.Fprintln(os.Stderr, "cupsctl:", err)
			os.Exit(1)
		}
		return
	}

	if err := applySettings(ctx, st, opts.updates); err != nil {
		fmt.Fprintln(os.Stderr, "cupsctl:", err)
		os.Exit(1)
	}
}

func usage(opt string) {
	if opt != "" {
		if strings.HasPrefix(opt, "-") {
			fmt.Fprintf(os.Stderr, "cupsctl: Unknown option %q\n", opt)
		} else {
			fmt.Fprintf(os.Stderr, "cupsctl: Unknown argument %q\n", opt)
		}
	}
	fmt.Println("Usage: cupsctl [options] [param=value ... paramN=valueN]")
	fmt.Println("Options:")
	fmt.Println("-E                      Encrypt the connection to the server")
	fmt.Println("-h server[:port]        Connect to the named server and port")
	fmt.Println("-U username             Specify username to use for authentication")
	fmt.Println("--[no-]debug-logging    Turn debug logging on/off")
	fmt.Println("--[no-]remote-admin     Turn remote administration on/off")
	fmt.Println("--[no-]remote-any       Allow/prevent access from the Internet")
	fmt.Println("--[no-]share-printers   Turn printer sharing on/off")
	fmt.Println("--[no-]user-cancel-any  Allow/prevent users to cancel any job")
	fmt.Println("--[no-]preserve-job-history  Preserve or clean completed jobs")
	fmt.Println("--[no-]preserve-job-files    Preserve or clean job files")
}

func parseArgs(args []string) (options, error) {
	opts := options{
		updates: map[string]string{},
	}

	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		if arg == "--help" {
			return opts, errShowHelp
		}

		if strings.HasPrefix(arg, "--") {
			key, val, ok := parseLongOption(arg)
			if !ok {
				return opts, fmt.Errorf("unknown option %q", arg)
			}
			opts.updates[key] = val
			continue
		}

		if strings.HasPrefix(arg, "-") && arg != "-" {
			short := strings.TrimPrefix(arg, "-")
			for pos := 0; pos < len(short); pos++ {
				ch := short[pos]
				rest := short[pos+1:]
				consume := func(name byte) (string, error) {
					if rest != "" {
						pos = len(short)
						return rest, nil
					}
					if i+1 >= len(args) {
						return "", fmt.Errorf("missing argument for -%c", name)
					}
					i++
					return args[i], nil
				}

				switch ch {
				case 'E':
					opts.encrypt = true
				case 'h':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.server = strings.TrimSpace(v)
				case 'U':
					v, err := consume(ch)
					if err != nil {
						return opts, err
					}
					opts.user = strings.TrimSpace(v)
				default:
					return opts, fmt.Errorf("unknown option \"-%c\"", ch)
				}
			}
			continue
		}

		if strings.Contains(arg, "=") {
			parts := strings.SplitN(arg, "=", 2)
			key := normalizeKey(parts[0])
			if key == "" {
				return opts, fmt.Errorf("invalid setting %q", arg)
			}
			opts.updates[key] = parts[1]
			continue
		}

		return opts, fmt.Errorf("unknown argument %q", arg)
	}

	return opts, nil
}

func parseLongOption(arg string) (string, string, bool) {
	switch arg {
	case "--share-printers":
		return "_share_printers", "1", true
	case "--no-share-printers":
		return "_share_printers", "0", true
	case "--debug-logging":
		return "_debug_logging", "1", true
	case "--no-debug-logging":
		return "_debug_logging", "0", true
	case "--remote-admin":
		return "_remote_admin", "1", true
	case "--no-remote-admin":
		return "_remote_admin", "0", true
	case "--remote-any":
		return "_remote_any", "1", true
	case "--no-remote-any":
		return "_remote_any", "0", true
	case "--remote-printers":
		return "_remote_printers", "1", true
	case "--no-remote-printers":
		return "_remote_printers", "0", true
	case "--user-cancel-any":
		return "_user_cancel_any", "1", true
	case "--no-user-cancel-any":
		return "_user_cancel_any", "0", true
	case "--preserve-job-history":
		return "_preserve_job_history", "1", true
	case "--no-preserve-job-history":
		return "_preserve_job_history", "0", true
	case "--preserve-job-files":
		return "_preserve_job_files", "1", true
	case "--no-preserve-job-files":
		return "_preserve_job_files", "0", true
	default:
		return "", "", false
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
		for k := range updates {
			if blocked, ok := blockedDirective(k); ok {
				return fmt.Errorf("cannot set %s directly", blocked)
			}
		}
		for k, v := range updates {
			if err := st.SetSetting(ctx, tx, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

func blockedDirective(key string) (string, bool) {
	for _, disallowed := range disallowedDirectives {
		if strings.EqualFold(strings.TrimSpace(key), disallowed) {
			return disallowed, true
		}
	}
	return "", false
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
