package config

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type LocationRule struct {
	Path         string
	AuthType     string
	RequireUser  bool
	RequireAdmin bool
	Order        string
	AllowAll     bool
	DenyAll      bool
	Allow        []string
	Deny         []string
	Limits       map[string]LimitRule
}

type Policy struct {
	Locations []LocationRule
	Policies  []string
	// PolicyLimits contains parsed <Policy name> blocks from cupsd.conf.
	// Keys are lower-cased policy names and lower-cased operation names.
	PolicyLimits map[string]map[string]LimitRule
}

type LimitRule struct {
	Ops          []string
	AuthType     string
	RequireUser  bool
	RequireOwner bool
	RequireAdmin bool
	Order        string
	AllowAll     bool
	DenyAll      bool
	Allow        []string
	Deny         []string
}

func LoadPolicy(confDir string) Policy {
	path := filepath.Join(confDir, "cupsd.conf")
	f, err := os.Open(path)
	if err != nil {
		return Policy{}
	}
	defer f.Close()

	policy := Policy{
		PolicyLimits: map[string]map[string]LimitRule{},
	}

	var curLoc *LocationRule
	var curLocLimit *LimitRule

	inPolicy := false
	curPolicyName := ""
	var curPolicyLimit *LimitRule

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "<Policy ") {
			name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "<Policy "), ">"))
			if name != "" {
				seen := false
				for _, existing := range policy.Policies {
					if strings.EqualFold(existing, name) {
						seen = true
						break
					}
				}
				if !seen {
					policy.Policies = append(policy.Policies, name)
				}
				inPolicy = true
				curPolicyName = name
				if policy.PolicyLimits[strings.ToLower(name)] == nil {
					policy.PolicyLimits[strings.ToLower(name)] = map[string]LimitRule{}
				}
			}
			continue
		}
		if line == "</Policy>" {
			inPolicy = false
			curPolicyName = ""
			curPolicyLimit = nil
			continue
		}

		if strings.HasPrefix(line, "<Location ") {
			p := strings.TrimSuffix(strings.TrimPrefix(line, "<Location "), ">")
			curLoc = &LocationRule{Path: p, Limits: map[string]LimitRule{}}
			continue
		}
		if line == "</Location>" {
			if curLoc != nil {
				policy.Locations = append(policy.Locations, *curLoc)
			}
			curLoc = nil
			curLocLimit = nil
			continue
		}

		if strings.HasPrefix(line, "<Limit ") && (curLoc != nil || inPolicy) {
			args := strings.TrimSuffix(strings.TrimPrefix(line, "<Limit "), ">")
			ops := strings.Fields(args)
			if inPolicy {
				curPolicyLimit = &LimitRule{Ops: ops}
			} else if curLoc != nil {
				curLocLimit = &LimitRule{Ops: ops}
			}
			continue
		}
		if line == "</Limit>" {
			if inPolicy && curPolicyName != "" && curPolicyLimit != nil {
				pkey := strings.ToLower(curPolicyName)
				if policy.PolicyLimits[pkey] == nil {
					policy.PolicyLimits[pkey] = map[string]LimitRule{}
				}
				for _, op := range curPolicyLimit.Ops {
					for _, alias := range opAliases(op) {
						policy.PolicyLimits[pkey][strings.ToLower(alias)] = *curPolicyLimit
					}
				}
				curPolicyLimit = nil
				continue
			}
			if curLoc != nil && curLocLimit != nil {
				for _, op := range curLocLimit.Ops {
					for _, alias := range opAliases(op) {
						curLoc.Limits[strings.ToLower(alias)] = *curLocLimit
					}
				}
				curLocLimit = nil
				continue
			}
		}

		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}

		// Apply settings to current limit (policy or location) or to current location.
		if inPolicy && curPolicyLimit != nil {
			limitCopy := *curPolicyLimit
			applyLimitDirective(&limitCopy, parts)
			*curPolicyLimit = limitCopy
			continue
		}
		if curLocLimit != nil {
			limitCopy := *curLocLimit
			applyLimitDirective(&limitCopy, parts)
			*curLocLimit = limitCopy
			continue
		}
		if curLoc != nil {
			applyLocationDirective(curLoc, parts)
		}
	}

	return policy
}

// PolicyLimitFor returns a policy-defined <Limit> rule for a given policy name
// and operation (e.g. "default"+"Print-Job"), if present.
func (p Policy) PolicyLimitFor(policyName string, op string) *LimitRule {
	if p.PolicyLimits == nil {
		return nil
	}
	policyName = strings.ToLower(strings.TrimSpace(policyName))
	op = strings.ToLower(strings.TrimSpace(op))
	if policyName == "" || op == "" {
		return nil
	}
	limits := p.PolicyLimits[policyName]
	if limits == nil {
		return nil
	}
	if lr, ok := limits[op]; ok {
		return &lr
	}
	for _, alias := range opAliases(op) {
		if lr, ok := limits[strings.ToLower(alias)]; ok {
			return &lr
		}
	}
	if lr, ok := limits["all"]; ok {
		return &lr
	}
	return nil
}

func (p Policy) Match(path string) *LocationRule {
	var best *LocationRule
	for i := range p.Locations {
		rule := &p.Locations[i]
		if strings.HasPrefix(path, rule.Path) {
			if best == nil || len(rule.Path) > len(best.Path) {
				best = rule
			}
		}
	}
	return best
}

func (p Policy) Allowed(path string, remoteIP string) bool {
	rule := p.Match(path)
	if rule == nil {
		return true
	}
	ip := net.ParseIP(remoteIP)
	return allowedByLists(rule.Order, ip, rule.AllowAll, rule.DenyAll, rule.Allow, rule.Deny)
}

// AllowedByLimit applies Allow/Deny lists from a <Limit> rule to a remote IP.
// It returns true when the IP is permitted by the rule.
func (p Policy) AllowedByLimit(limit *LimitRule, remoteIP string) bool {
	if limit == nil {
		return true
	}
	ip := net.ParseIP(remoteIP)
	return allowedByLists(limit.Order, ip, limit.AllowAll, limit.DenyAll, limit.Allow, limit.Deny)
}

func (p Policy) LimitFor(path string, op string) *LimitRule {
	rule := p.Match(path)
	if rule == nil {
		return nil
	}
	if rule.Limits == nil {
		return nil
	}
	op = strings.ToLower(strings.TrimSpace(op))
	if lr, ok := rule.Limits[op]; ok {
		return &lr
	}
	for _, alias := range opAliases(op) {
		if lr, ok := rule.Limits[strings.ToLower(alias)]; ok {
			return &lr
		}
	}
	if lr, ok := rule.Limits["all"]; ok {
		return &lr
	}
	return nil
}

func applyLocationDirective(target *LocationRule, parts []string) {
	if target == nil || len(parts) == 0 {
		return
	}
	switch parts[0] {
	case "AuthType":
		if len(parts) > 1 {
			target.AuthType = parts[1]
		}
	case "Require":
		if len(parts) > 1 {
			if parts[1] == "user" {
				target.RequireUser = true
				for _, token := range parts[2:] {
					if token == "@SYSTEM" || token == "admin" {
						target.RequireAdmin = true
					}
				}
			} else if parts[1] == "group" {
				target.RequireAdmin = true
			}
		}
	case "Order":
		if len(parts) > 1 {
			target.Order = parts[1]
		}
	case "Allow":
		if len(parts) > 2 && parts[1] == "from" {
			if parts[2] == "all" {
				target.AllowAll = true
			} else {
				target.Allow = append(target.Allow, parts[2:]...)
			}
		}
	case "Deny":
		if len(parts) > 2 && parts[1] == "from" {
			if parts[2] == "all" {
				target.DenyAll = true
			} else {
				target.Deny = append(target.Deny, parts[2:]...)
			}
		}
	}
}

func applyLimitDirective(target *LimitRule, parts []string) {
	if target == nil || len(parts) == 0 {
		return
	}
	switch parts[0] {
	case "AuthType":
		if len(parts) > 1 {
			target.AuthType = parts[1]
		}
	case "Require":
		if len(parts) > 1 {
			if parts[1] == "user" {
				target.RequireUser = true
				for _, token := range parts[2:] {
					switch token {
					case "@OWNER":
						target.RequireOwner = true
					case "@SYSTEM", "admin":
						target.RequireAdmin = true
					}
				}
			} else if parts[1] == "group" {
				target.RequireAdmin = true
			}
		}
	case "Order":
		if len(parts) > 1 {
			target.Order = parts[1]
		}
	case "Allow":
		if len(parts) > 2 && parts[1] == "from" {
			if parts[2] == "all" {
				target.AllowAll = true
			} else {
				target.Allow = append(target.Allow, parts[2:]...)
			}
		}
	case "Deny":
		if len(parts) > 2 && parts[1] == "from" {
			if parts[2] == "all" {
				target.DenyAll = true
			} else {
				target.Deny = append(target.Deny, parts[2:]...)
			}
		}
	}
}

func allowedByLists(order string, ip net.IP, allowAll bool, denyAll bool, allow, deny []string) bool {
	if denyAll {
		return false
	}
	if allowAll {
		return true
	}
	if ip == nil {
		return true
	}
	ord := strings.ToLower(order)
	if ord == "" {
		ord = "deny,allow"
	}
	allowMatch := ipMatches(ip, allow)
	denyMatch := ipMatches(ip, deny)
	if ord == "allow,deny" {
		if allowMatch {
			if denyMatch {
				return false
			}
			return true
		}
		return false
	}
	// deny,allow
	if denyMatch {
		return false
	}
	if len(allow) == 0 {
		return true
	}
	return allowMatch
}

func ipMatches(ip net.IP, rules []string) bool {
	if len(rules) == 0 {
		return false
	}
	for _, r := range rules {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		switch strings.ToLower(r) {
		case "localhost":
			if ip.IsLoopback() {
				return true
			}
		case "@local":
			if isPrivate(ip) {
				return true
			}
		default:
			if strings.Contains(r, "/") {
				if _, cidr, err := net.ParseCIDR(r); err == nil && cidr.Contains(ip) {
					return true
				}
			} else {
				if ip.Equal(net.ParseIP(r)) {
					return true
				}
			}
		}
	}
	return false
}

func isPrivate(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	privateCIDRs := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	for _, cidr := range privateCIDRs {
		_, c, _ := net.ParseCIDR(cidr)
		if c != nil && c.Contains(ip) {
			return true
		}
	}
	return false
}

func opAliases(op string) []string {
	op = strings.TrimSpace(op)
	if op == "" {
		return nil
	}
	aliases := []string{op}
	lower := strings.ToLower(op)
	// CUPS config uses singular "Create-Job-Subscription" while IPP uses plural.
	if strings.HasSuffix(lower, "subscriptions") {
		aliases = append(aliases, op[:len(op)-1])
	} else if strings.HasSuffix(lower, "subscription") {
		aliases = append(aliases, op+"s")
	}
	return aliases
}

