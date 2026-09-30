package link

import (
	"net/http"
	"strings"
	"testing"
)

func TestStripHopKeepsEndToEndHeaders(t *testing.T) {
	header := http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Te": {"trailers"}, "Authorization": {"Bearer x"}, "Accept": {"application/json"}}
	StripHop(header)
	if len(header) != 2 || header.Get("Authorization") != "Bearer x" || header.Get("Accept") != "application/json" {
		t.Fatalf("stripped header = %v", header)
	}
}

func TestValidUpstreamNameIsOneLabel(t *testing.T) {
	for _, name := range []string{"vm", "vm-1", "a", strings.Repeat("a", 63)} {
		if !ValidUpstreamName(name) {
			t.Errorf("%q refused", name)
		}
	}
	for _, name := range []string{"", "VM", "vm.internal", "-vm", "vm-", "vm_1", strings.Repeat("a", 64)} {
		if ValidUpstreamName(name) {
			t.Errorf("%q accepted", name)
		}
	}
}

func TestExpandEnvNamesEveryUnsetVariable(t *testing.T) {
	t.Setenv("LINK_TEST_TOKEN", "tenant-seven")
	out, err := ExpandEnv([]byte("token: ${LINK_TEST_TOKEN}\nheader: Bearer $LINK_TEST_TOKEN\n"))
	if err != nil || string(out) != "token: tenant-seven\nheader: Bearer tenant-seven\n" {
		t.Fatalf("expanded %q, %v", out, err)
	}
	_, err = ExpandEnv([]byte("a: ${LINK_TEST_MISSING_B}\nb: ${LINK_TEST_MISSING_A}\nc: ${LINK_TEST_TOKEN}\n"))
	if err == nil || !strings.HasSuffix(err.Error(), "LINK_TEST_MISSING_A, LINK_TEST_MISSING_B") {
		t.Fatalf("unset variables: %v", err)
	}
	// An unterminated reference is an error, never a silently shorter file.
	if out, err := ExpandEnv([]byte("token: ${LINK_TEST_TOKEN\nupstreams: []\n")); err == nil || !strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("unterminated reference: %q, %v", out, err)
	}
}

func TestStripHopRemovesProxyCredentials(t *testing.T) {
	header := http.Header{"Proxy-Authorization": {"Basic x"}, "Proxy-Authenticate": {"Basic"}, "Accept": {"*/*"}}
	StripHop(header)
	if len(header) != 1 || header.Get("Accept") != "*/*" {
		t.Fatalf("stripped header = %v", header)
	}
}
