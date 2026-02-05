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
}

type LimitRule struct {
	Ops          []string
	AuthType     string
	RequireUser  bool
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

	var policy Policy
	var cur *LocationRule
	var curLimit *LimitRule
	inPolicy := false
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
				for _, p := range policy.Policies {
					if strings.EqualFold(p, name) {
						seen = true
						break
					}
				}
				if !seen {
					policy.Policies = append(policy.Policies, name)
				}
			}
			inPolicy = true
			continue
		}
		if line == "</Policy>" {
			inPolicy = false
			continue
		}
		if inPolicy {
			continue
		}
		if strings.HasPrefix(line, "<Location ") {
			p := strings.TrimSuffix(strings.TrimPrefix(line, "<Location "), ">")
			cur = &LocationRule{Path: p, Limits: map[string]LimitRule{}}
			continue
		}
		if line == "</Location>" {
			if cur != nil {
				policy.Locations = append(policy.Locations, *cur)
			}
			cur = nil
			continue
		}
		if strings.HasPrefix(line, "<Limit ") && cur != nil {
			args := strings.TrimSuffix(strings.TrimPrefix(line, "<Limit "), ">")
			ops := strings.Fields(args)
			curLimit = &LimitRule{Ops: ops}
			continue
		}
		if line == "</Limit>" && cur != nil && curLimit != nil {
			for _, op := range curLimit.Ops {
				cur.Limits[strings.ToLower(op)] = *curLimit
			}
			curLimit = nil
			continue
		}
		if cur == nil {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		target := cur
		if curLimit != nil {
			// Apply settings to current limit
			limitCopy := *curLimit
			target = nil
			switch parts[0] {
			case "AuthType":
				if len(parts) > 1 {
					limitCopy.AuthType = parts[1]
				}
			case "Require":
				if len(parts) > 1 {
					if parts[1] == "user" {
						limitCopy.RequireUser = true
						if len(parts) > 2 && (parts[2] == "@SYSTEM" || parts[2] == "admin") {
							limitCopy.RequireAdmin = true
						}
					} else if parts[1] == "group" {
						limitCopy.RequireAdmin = true
					}
				}
			case "Order":
				if len(parts) > 1 {
					limitCopy.Order = parts[1]
				}
			case "Allow":
				if len(parts) > 2 && parts[1] == "from" {
					if parts[2] == "all" {
						limitCopy.AllowAll = true
					} else {
						limitCopy.Allow = append(limitCopy.Allow, parts[2:]...)
					}
				}
			case "Deny":
				if len(parts) > 2 && parts[1] == "from" {
					if parts[2] == "all" {
						limitCopy.DenyAll = true
					} else {
						limitCopy.Deny = append(limitCopy.Deny, parts[2:]...)
					}
				}
			}
			*curLimit = limitCopy
			continue
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
					if len(parts) > 2 && (parts[2] == "@SYSTEM" || parts[2] == "admin") {
						target.RequireAdmin = true
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
	return policy
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

func (p Policy) LimitFor(path string, op string) *LimitRule {
	rule := p.Match(path)
	if rule == nil {
		return nil
	}
	if rule.Limits == nil {
		return nil
	}
	if lr, ok := rule.Limits[strings.ToLower(op)]; ok {
		return &lr
	}
	if lr, ok := rule.Limits["all"]; ok {
		return &lr
	}
	return nil
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
