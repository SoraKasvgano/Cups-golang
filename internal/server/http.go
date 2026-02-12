package server

import (
	"log"
	"net"
	"net/http"
	"strings"

	"cupsgolang/internal/config"
	"cupsgolang/internal/spool"
	"cupsgolang/internal/store"
	"cupsgolang/internal/web"
)

type Server struct {
	Config config.Config
	Store  *store.Store
	Spool  spool.Spool
	Policy config.Policy
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Config.MaxRequestSize > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, s.Config.MaxRequestSize)
		}
		remoteIP := r.RemoteAddr
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			remoteIP = host
		}
		if rule := s.Policy.Match(r.URL.Path); rule != nil {
			if !s.Policy.Allowed(r.URL.Path, remoteIP) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			if limit := s.Policy.LimitFor(r.URL.Path, r.Method); limit != nil {
				if limit.DenyAll {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
				if limitRequiresAuth(limit) {
					authType := s.authTypeForRequest(r, r.Method)
					u, ok := s.authenticate(r, authType)
					if !ok {
						setAuthChallenge(w, authType)
						http.Error(w, "Unauthorized", http.StatusUnauthorized)
						return
					}
					if !userAllowedByLimit(u, "", limit) {
						http.Error(w, "Forbidden", http.StatusForbidden)
						return
					}
				}
			} else if locationRequiresAuth(rule) {
				authType := s.authTypeForRequest(r, r.Method)
				u, ok := s.authenticate(r, authType)
				if !ok {
					setAuthChallenge(w, authType)
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				if !userAllowedByLocation(u, rule) {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			}
		}
		switch {
		case r.URL.Path == "/":
			s.handleRoot(w, r)
		case r.URL.Path == "/cups.css" || r.URL.Path == "/cups-printable.css" || r.URL.Path == "/apple-touch-icon.png" || r.URL.Path == "/robots.txt" || strings.HasPrefix(r.URL.Path, "/images/") || strings.HasPrefix(r.URL.Path, "/strings/"):
			web.CupsAssetHandler().ServeHTTP(w, r)
		case r.URL.Path == "/help" || r.URL.Path == "/help/":
			web.RenderHelp(w, r)
		case strings.HasPrefix(r.URL.Path, "/help/"):
			web.CupsHelpHandler().ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/ui/"):
			http.StripPrefix("/ui/", web.AssetHandler()).ServeHTTP(w, r)
		case r.URL.Path == "/admin" || r.URL.Path == "/admin/":
			s.handleAdmin(w, r)
		case r.URL.Path == "/classes" || r.URL.Path == "/classes/":
			web.RenderClasses(w, r, s.Store)
		case strings.HasPrefix(r.URL.Path, "/classes/"):
			if r.Method == http.MethodPost {
				s.handleClassPost(w, r)
				return
			}
			web.RenderClass(w, r, s.Store)
		case r.URL.Path == "/printers" || r.URL.Path == "/printers/":
			s.handlePrinters(w, r)
		case strings.HasPrefix(r.URL.Path, "/printers/"):
			if r.Method == http.MethodPost {
				s.handlePrinterPost(w, r)
				return
			}
			s.handlePrinter(w, r)
		case r.URL.Path == "/ipp/print":
			s.handleIPP(w, r)
		case r.URL.Path == "/jobs" || r.URL.Path == "/jobs/":
			if r.Method == http.MethodPost {
				s.handleJobsPost(w, r)
				return
			}
			s.handleJobs(w, r)
		case strings.HasPrefix(r.URL.Path, "/jobs/"):
			s.handleJob(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/printers/", http.StatusFound)
}

func (s *Server) handlePrinters(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && isIPP(r) {
		s.handleIPP(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	web.RenderPrinters(w, r, s.Store)
}

func (s *Server) handlePrinter(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && isIPP(r) {
		s.handleIPP(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	web.RenderPrinter(w, r, s.Store)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && isIPP(r) {
		s.handleIPP(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	web.RenderJobs(w, r, s.Store)
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && isIPP(r) {
		s.handleIPP(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	web.RenderJob(w, r, s.Store)
}

func (s *Server) handleIPP(w http.ResponseWriter, r *http.Request) {
	if !isIPP(r) {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}
	if err := s.handleIPPRequest(w, r); err != nil {
		log.Printf("IPP error: %v", err)
	}
}

func isIPP(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "application/ipp")
}
