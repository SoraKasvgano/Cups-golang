package server

import (
	"net/http"
	"strings"

	goipp "github.com/OpenPrinting/goipp"

	"cupsgolang/internal/config"
)

// authInfoRequiredForDestination computes the "auth-info-required" values (if any)
// for a printer/class URI, following the same precedence as CUPS:
//  1) <Location>/<Limit POST> rules for the printer/class resource
//  2) Printer/class operation policy for Print-Job
//
// The returned values are used for:
//  - auth-info-required
//  - uri-authentication-supported
//  - DNS-SD "air=" TXT record
func (s *Server) authInfoRequiredForDestination(isClass bool, name string, defaultOptionsJSON string) []string {
	if s == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	resource := "/printers/" + name
	if isClass {
		resource = "/classes/" + name
	}

	// First, check the per-location POST requirements.
	if auth := s.authInfoRequiredForLocation(resource, http.MethodPost); len(auth) > 0 {
		return auth
	}

	// Otherwise, fall back to the queue's operation policy for Print-Job.
	policyName := s.policyNameForDefaultOptions(defaultOptionsJSON)
	limit := s.Policy.PolicyLimitFor(policyName, goipp.OpPrintJob.String())
	if limit == nil && !strings.EqualFold(policyName, s.defaultPolicyName()) {
		// Unknown policy references fall back to default policy (CUPS behavior).
		limit = s.Policy.PolicyLimitFor(s.defaultPolicyName(), goipp.OpPrintJob.String())
	}
	if limit == nil {
		return nil
	}

	if !limitRequiresAuth(limit) {
		return nil
	}

	authType := s.normalizeAuthType(limit.AuthType)
	if strings.TrimSpace(authType) == "" {
		authType = s.defaultAuthTypeFallback()
	}
	if strings.EqualFold(strings.TrimSpace(authType), "none") {
		return nil
	}
	return authInfoRequiredFromAuthType(authType)
}

func (s *Server) authInfoRequiredForLocation(path string, method string) []string {
	if s == nil {
		return nil
	}
	path = strings.TrimSpace(path)
	method = strings.TrimSpace(method)
	if path == "" || method == "" {
		return nil
	}

	rule := s.Policy.Match(path)
	if rule == nil {
		return nil
	}

	// Check the most specific <Limit METHOD> first.
	if limit := s.Policy.LimitFor(path, method); limit != nil {
		if !limitRequiresAuth(limit) {
			return nil
		}
		authType := s.normalizeAuthType(limit.AuthType)
		if strings.TrimSpace(authType) == "" {
			authType = s.defaultAuthTypeFallback()
		}
		if strings.EqualFold(strings.TrimSpace(authType), "none") {
			return nil
		}
		return authInfoRequiredFromAuthType(authType)
	}

	// Then fall back to the <Location> rule itself.
	if !locationRequiresAuth(rule) {
		return nil
	}
	authType := s.normalizeAuthType(rule.AuthType)
	if strings.TrimSpace(authType) == "" {
		authType = s.defaultAuthTypeFallback()
	}
	if strings.EqualFold(strings.TrimSpace(authType), "none") {
		return nil
	}
	return authInfoRequiredFromAuthType(authType)
}

func (s *Server) defaultAuthTypeFallback() string {
	if s == nil {
		return "basic"
	}
	if v := strings.TrimSpace(s.Config.DefaultAuthType); v != "" && !strings.EqualFold(v, "default") {
		return v
	}
	// CUPS defaults to Basic when authentication is required but no explicit
	// AuthType/DefaultAuthType is configured.
	return "basic"
}

func limitRequiresAuth(limit *config.LimitRule) bool {
	if limit == nil {
		return false
	}
	if limit.RequireUser || limit.RequireOwner || limit.RequireAdmin {
		return true
	}
	if len(limit.RequireUsers) > 0 || len(limit.RequireGroups) > 0 {
		return true
	}
	return false
}

func locationRequiresAuth(rule *config.LocationRule) bool {
	if rule == nil {
		return false
	}
	if rule.RequireUser || rule.RequireAdmin {
		return true
	}
	if len(rule.RequireUsers) > 0 || len(rule.RequireGroups) > 0 {
		return true
	}
	return false
}

