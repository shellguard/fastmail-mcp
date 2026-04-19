package main

import (
	"testing"
)

func TestIsSafeExternalURL_BlocksLocalhost(t *testing.T) {
	urls := []string{
		"https://localhost/path",
		"https://127.0.0.1/path",
		"https://[::1]/path",
	}
	for _, u := range urls {
		if err := isSafeExternalURL(u); err == nil {
			t.Errorf("expected %q to be blocked", u)
		}
	}
}

func TestIsSafeExternalURL_BlocksPrivateNetworks(t *testing.T) {
	urls := []string{
		"https://10.0.0.1/path",
		"https://192.168.1.1/path",
		"https://172.16.0.1/path",
	}
	for _, u := range urls {
		if err := isSafeExternalURL(u); err == nil {
			t.Errorf("expected %q to be blocked", u)
		}
	}
}

func TestIsSafeExternalURL_BlocksMetadata(t *testing.T) {
	if err := isSafeExternalURL("https://169.254.169.254/latest/meta-data"); err == nil {
		t.Error("expected cloud metadata IP to be blocked")
	}
}

func TestIsSafeExternalURL_BlocksHTTP(t *testing.T) {
	if err := isSafeExternalURL("http://example.com/unsubscribe"); err == nil {
		t.Error("expected non-HTTPS to be blocked")
	}
}

func TestIsSafeExternalURL_AllowsPublic(t *testing.T) {
	if err := isSafeExternalURL("https://example.com/unsubscribe"); err != nil {
		t.Errorf("expected public URL to be allowed, got: %v", err)
	}
}
