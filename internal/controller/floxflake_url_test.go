package controller

import (
	"strings"
	"testing"
)

func TestNodeReachableURL(t *testing.T) {
	// A URL already using an IP is returned unchanged (nothing to resolve).
	const ipURL = "http://10.48.180.165:80/gitrepository/flux-system/flox-catalogue/deadbeef.tar.gz"
	if got, err := nodeReachableURL(ipURL); err != nil || got != ipURL {
		t.Errorf("ip passthrough: got %q err %v, want unchanged", got, err)
	}

	// A hostname is resolved to an IP; scheme/port/path are preserved and the DNS name is gone.
	// localhost is reliably resolvable in any environment.
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

	if _, err := nodeReachableURL("://not a url"); err == nil {
		t.Error("expected error on an unparseable URL")
	}
}
