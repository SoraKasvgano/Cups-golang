package main

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cupsgolang/internal/config"
	"cupsgolang/internal/scheduler"
	"cupsgolang/internal/server"
	"cupsgolang/internal/spool"
	"cupsgolang/internal/store"
	"cupsgolang/internal/tlsutil"
)

func main() {
	cfg := config.Load()
	server.SetAppConfig(cfg)

	var logFile *os.File
	if cfg.ErrorLogPath != "" && !strings.EqualFold(cfg.ErrorLogPath, "syslog") {
		if err := os.MkdirAll(filepath.Dir(cfg.ErrorLogPath), 0755); err != nil {
			log.Printf("warning: failed to create log dir: %v", err)
		} else {
			if f, err := os.OpenFile(cfg.ErrorLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
				logFile = f
				log.SetOutput(f)
			} else {
				log.Printf("warning: failed to open error log: %v", err)
			}
		}
	}
	if logFile != nil {
		defer logFile.Close()
	}

	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		log.Fatalf("failed to create data dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0755); err != nil {
		log.Fatalf("failed to create db dir: %v", err)
	}
	if err := os.MkdirAll(cfg.ConfDir, 0755); err != nil {
		log.Fatalf("failed to create conf dir: %v", err)
	}
	if err := os.MkdirAll(cfg.PPDDir, 0755); err != nil {
		log.Fatalf("failed to create ppd dir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	if err := st.EnsureDefaultPrinter(ctx); err != nil {
		log.Fatalf("failed to ensure default printer: %v", err)
	}
	if err := st.EnsureAdminUser(ctx); err != nil {
		log.Fatalf("failed to ensure admin user: %v", err)
	}
	if err := config.SyncFromConf(ctx, cfg.ConfDir, st); err != nil {
		log.Printf("warning: failed to sync from conf: %v", err)
	}
	config.SyncLoop(ctx, cfg.ConfDir, st)
	mimeDB, err := config.LoadMimeDB(cfg.ConfDir)
	if err != nil {
		log.Printf("warning: failed to load mime db: %v", err)
	}

	sp := spool.Spool{Dir: cfg.SpoolDir, OutputDir: cfg.OutputDir}
	if err := sp.Ensure(); err != nil {
		log.Fatalf("failed to ensure spool dir: %v", err)
	}

	sched := &scheduler.Scheduler{Store: st, Spool: sp, Interval: 2 * time.Second, Mime: mimeDB, Config: cfg}
	sched.Start(ctx)
	defer sched.Stop()

	policy := config.LoadPolicy(cfg.ConfDir)
	srv := &server.Server{Config: cfg, Store: st, Spool: sp, Policy: policy}

	handler := srv.Handler()
	newServer := func(addr string) *http.Server {
		return &http.Server{
			Addr:         addr,
			Handler:      handler,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  60 * time.Second,
		}
	}

	var servers []*http.Server
	var listeners []net.Listener

	listenHTTP := append([]string{}, cfg.ListenHTTP...)
	listenHTTPS := append([]string{}, cfg.ListenHTTPS...)
	if len(listenHTTP) == 0 && len(listenHTTPS) == 0 && strings.TrimSpace(cfg.ListenAddr) != "" {
		listenHTTP = []string{cfg.ListenAddr}
	}
	if cfg.TLSOnly {
		if len(listenHTTPS) == 0 {
			listenHTTPS = listenHTTP
		}
		listenHTTP = nil
	}
	listenHTTP = uniqueAddrs(listenHTTP)
	listenHTTPS = uniqueAddrs(listenHTTPS)

	var tlsConfig *tls.Config
	if cfg.TLSEnabled {
		hostname, _ := os.Hostname()
		hosts := uniqueAddrs(append([]string{"localhost", cfg.ServerName, hostname}, cfg.ServerAlias...))
		certHosts := uniqueHosts(hosts)
		cert, err := tlsutil.EnsureCertificate(cfg.TLSCertPath, cfg.TLSKeyPath, certHosts, cfg.TLSAutoGenerate)
		if err != nil {
			log.Fatalf("failed to load TLS certificate: %v", err)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	startServe := func(addr string, ln net.Listener, label string) {
		srv := newServer(addr)
		servers = append(servers, srv)
		listeners = append(listeners, ln)
		go func() {
			log.Printf("CUPS-Golang %s listening on %s", label, addr)
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Fatalf("listen error: %v", err)
			}
		}()
	}

	splitTLS := cfg.TLSEnabled && !cfg.TLSOnly && len(listenHTTPS) == 0
	if splitTLS {
		for _, addr := range listenHTTP {
			baseLn, err := net.Listen("tcp", addr)
			if err != nil {
				log.Fatalf("listen error on %s: %v", addr, err)
			}
			plainLn, tlsLn := tlsutil.SplitListener(baseLn, tlsConfig, true)
			startServe(addr, plainLn, "HTTP")
			startServe(addr, tlsLn, "HTTPS")
			listeners = append(listeners, baseLn)
		}
	} else {
		for _, addr := range listenHTTP {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				log.Fatalf("listen error on %s: %v", addr, err)
			}
			startServe(addr, ln, "HTTP")
		}
		if cfg.TLSEnabled {
			for _, addr := range listenHTTPS {
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					log.Fatalf("listen error on %s: %v", addr, err)
				}
				startServe(addr, tls.NewListener(ln, tlsConfig), "HTTPS")
			}
		} else if len(listenHTTPS) > 0 {
			log.Printf("TLS disabled; skipping HTTPS listeners: %v", listenHTTPS)
		}
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	<-sigs

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
	for _, ln := range listeners {
		_ = ln.Close()
	}
}

func uniqueAddrs(addrs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, addr)
	}
	return out
}

func uniqueHosts(hosts []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		host = stripPort(host)
		if host == "" {
			continue
		}
		if seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	return out
}

func stripPort(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if strings.HasPrefix(host, "[") {
		if h, _, err := net.SplitHostPort(host); err == nil {
			return strings.Trim(h, "[]")
		}
		return strings.Trim(host, "[]")
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
