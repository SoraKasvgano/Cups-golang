package backend

import (
	"errors"
	"net/url"
	"testing"
)

func TestClassifyDNSSDResolveError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{name: "nil", err: nil, want: ErrorTemporary},
		{name: "unsupported sentinel", err: ErrUnsupported, want: ErrorUnsupported},
		{name: "invalid uri", err: errors.New("invalid dnssd uri"), want: ErrorUnsupported},
		{name: "empty uri", err: errors.New("empty dnssd uri"), want: ErrorUnsupported},
		{name: "unsupported text", err: errors.New("unsupported service"), want: ErrorUnsupported},
		{name: "url parse", err: &url.Error{Op: "parse", URL: "dnssd://", Err: errors.New("bad uri")}, want: ErrorUnsupported},
		{name: "temporary timeout", err: errors.New("dnssd resolution timeout"), want: ErrorTemporary},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyDNSSDResolveError(tc.err); got != tc.want {
				t.Fatalf("classifyDNSSDResolveError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestParseDNSSDURI(t *testing.T) {
	instance, service, domain, err := parseDNSSDURI("dnssd://Office%20Printer._ipp._tcp.local/")
	if err != nil {
		t.Fatalf("parseDNSSDURI(valid) error: %v", err)
	}
	if instance != "Office Printer" || service != "_ipp._tcp" || domain != "local" {
		t.Fatalf("parseDNSSDURI(valid) = %q %q %q", instance, service, domain)
	}

	if _, _, _, err := parseDNSSDURI("dnssd://"); err == nil {
		t.Fatal("parseDNSSDURI(empty host) expected error")
	}
}

func TestBuildAndParseDNSSDURI_RoundTripEscapedInstance(t *testing.T) {
	uri := buildDNSSDDeviceURI("_ipp._tcp", "Office Printer", "local", map[string]string{"uuid": "abc-123"})
	instance, service, domain, err := parseDNSSDURI(uri)
	if err != nil {
		t.Fatalf("round-trip parse error: %v", err)
	}
	if instance != "Office Printer" || service != "_ipp._tcp" || domain != "local" {
		t.Fatalf("round-trip parse = %q %q %q, uri=%q", instance, service, domain, uri)
	}
}
