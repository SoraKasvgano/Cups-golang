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

	baseLn, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatalf("listen error: %v", err)
	}

	var servers []*http.Server
	var listeners []net.Listener

	if cfg.TLSEnabled {
		hostname, _ := os.Hostname()
		hosts := []string{"localhost", cfg.ServerName, hostname}
		cert, err := tlsutil.EnsureCertificate(cfg.TLSCertPath, cfg.TLSKeyPath, hosts, cfg.TLSAutoGenerate)
		if err != nil {
			log.Fatalf("failed to load TLS certificate: %v", err)
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}

		if cfg.TLSOnly {
			tlsLn := tls.NewListener(baseLn, tlsConfig)
			httpsSrv := newServer(cfg.ListenAddr)
			servers = append(servers, httpsSrv)
			listeners = append(listeners, tlsLn)
			go func() {
				log.Printf("CUPS-Golang HTTPS listening on %s", cfg.ListenAddr)
				if err := httpsSrv.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
					log.Fatalf("listen error: %v", err)
				}
			}()
		} else {
			plainLn, tlsLn := tlsutil.SplitListener(baseLn, tlsConfig, true)
			httpSrv := newServer(cfg.ListenAddr)
			httpsSrv := newServer(cfg.ListenAddr)
			servers = append(servers, httpSrv, httpsSrv)
			listeners = append(listeners, plainLn, tlsLn, baseLn)
			go func() {
				log.Printf("CUPS-Golang HTTP listening on %s", cfg.ListenAddr)
				if err := httpSrv.Serve(plainLn); err != nil && err != http.ErrServerClosed {
					log.Fatalf("listen error: %v", err)
				}
			}()
			go func() {
				log.Printf("CUPS-Golang HTTPS listening on %s", cfg.ListenAddr)
				if err := httpsSrv.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
					log.Fatalf("listen error: %v", err)
				}
			}()
		}
	} else {
		httpSrv := newServer(cfg.ListenAddr)
		servers = append(servers, httpSrv)
		listeners = append(listeners, baseLn)
		go func() {
			log.Printf("CUPS-Golang HTTP listening on %s", cfg.ListenAddr)
			if err := httpSrv.Serve(baseLn); err != nil && err != http.ErrServerClosed {
				log.Fatalf("listen error: %v", err)
			}
		}()
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
