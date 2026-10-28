package controller

import (
	"net"
	"strings"
	"testing"
)

func TestNodeReachableURL(t *testing.T) {
	// A URL already using an IP is returned unchanged (nothing to resolve).
	const ipURL = "http://10.48.180.165:80/gitrepository/flux-system/flox-catalogue/deadbeef.tar.gz"
	if got, err := nodeReachableURL(ipURL); err != nil || got != ipURL {
		t.Errorf("ip passthrough: got %q err %v, want unchanged", got, err)
	}

	if _, err := nodeReachableURL("://not a url"); err == nil {
		t.Error("expected error on an unparseable URL")
	}

	// A hostname is resolved to an IP; scheme/port/path are preserved and the DNS name is gone.
	// The nix build sandbox has no resolver, not even for localhost, so this half needs one.
	if _, err := net.LookupHost("localhost"); err != nil {
		t.Skipf("no resolver here (%v): hostname rewrite not exercised", err)
	}
	got, err := nodeReachableURL("http://localhost:8080/gitrepository/x/y.tar.gz")
	if err != nil {
		t.Fatalf("resolve localhost: %v", err)
	}
	if strings.Contains(got, "localhost") {
		t.Errorf("hostname not rewritten: %q", got)
	}
	if !strings.HasPrefix(got, "http://") || !strings.HasSuffix(got, ":8080/gitrepository/x/y.tar.gz") {
		t.Errorf("scheme/port/path not preserved: %q", got)
	}
}
