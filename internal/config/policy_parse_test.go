package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPolicyCaseInsensitiveParsing(t *testing.T) {
	confDir := t.TempDir()
	cupsdConf := `
<policy default>
  <limit Print-Job CUPS-Add-Modify-Printer>
    authtype Basic
    require user @OWNER alice
    order deny,allow
    allow from 127.0.0.1
  </limit>
</policy>

<Location /admin>
  authtype Digest
  require group @SYSTEM printops
  order allow,deny
  deny from all
</Location>

<Location /public>
  require all granted
</Location>

<Location /disabled>
  require all denied
</Location>
`
	if err := os.WriteFile(filepath.Join(confDir, "cupsd.conf"), []byte(cupsdConf), 0o644); err != nil {
		t.Fatalf("write cupsd.conf: %v", err)
	}

	policy := LoadPolicy(confDir)
	if len(policy.Policies) == 0 {
		t.Fatalf("expected parsed policy names")
	}
	limit := policy.PolicyLimitFor("default", "Print-Job")
	if limit == nil {
		t.Fatalf("missing policy limit for default/Print-Job")
	}
	if limit.AuthType != "Basic" {
		t.Fatalf("AuthType = %q, want Basic", limit.AuthType)
	}
	if !limit.RequireOwner {
		t.Fatalf("RequireOwner = false, want true")
	}
	if len(limit.RequireUsers) != 1 || limit.RequireUsers[0] != "alice" {
		t.Fatalf("RequireUsers = %#v, want [alice]", limit.RequireUsers)
	}
	if !limit.AllowAll && len(limit.Allow) == 0 {
		t.Fatalf("expected allow list to be parsed")
	}

	adminRule := policy.Match("/admin")
	if adminRule == nil {
		t.Fatalf("expected /admin rule")
	}
	if adminRule.AuthType != "Digest" {
		t.Fatalf("/admin AuthType = %q, want Digest", adminRule.AuthType)
	}
	if !adminRule.RequireAdmin {
		t.Fatalf("/admin RequireAdmin = false, want true")
	}
	if !adminRule.DenyAll {
		t.Fatalf("/admin DenyAll = false, want true")
	}

	publicRule := policy.Match("/public")
	if publicRule == nil {
		t.Fatalf("expected /public rule")
	}
	if publicRule.RequireUser || publicRule.RequireAdmin || len(publicRule.RequireUsers) > 0 || len(publicRule.RequireGroups) > 0 {
		t.Fatalf("/public should not require auth, got %+v", *publicRule)
	}

	disabledRule := policy.Match("/disabled")
	if disabledRule == nil {
		t.Fatalf("expected /disabled rule")
	}
	if !disabledRule.DenyAll {
		t.Fatalf("/disabled DenyAll = false, want true")
	}
}
