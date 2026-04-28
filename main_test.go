package main

import (
	"net"
	"strings"
	"testing"
)

func TestStripANSICodes(t *testing.T) {
	input := "hello \x1b[0;32mgreen\x1b[0m world"
	want := "hello green world"
	if got := stripANSICodes(input); got != want {
		t.Fatalf("stripANSICodes() = %q, want %q", got, want)
	}
}

func TestPasswordCacheGetSet(t *testing.T) {
	cache := NewPasswordCache("")
	if cache.Get() != "" {
		t.Fatalf("expected empty cache")
	}
	cache.Set("secret")
	if cache.Get() != "secret" {
		t.Fatalf("expected cached value to be updated")
	}
}

func TestPasswordResponderUsesCacheOnce(t *testing.T) {
	cache := NewPasswordCache("secret")
	responder := NewPasswordResponder(cache)

	got, ok := responder.Next(nil, "SSH password")
	if !ok || got != "secret" {
		t.Fatalf("expected cached password on first use")
	}

	got, ok = responder.Next(nil, "SSH password")
	if ok || got != "" {
		t.Fatalf("expected responder to require a prompt after cache is used")
	}
}

func TestEnsureLocalPortAvailableDetectsUsedPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("sandbox does not permit local port binding: %v", err)
		}
		t.Fatalf("failed to reserve test port: %v", err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	if err := ensureLocalPortAvailable(port); err == nil {
		t.Fatalf("expected occupied port %d to be reported unavailable", port)
	}
}

func TestEnsureLocalPortAvailableAllowsFreePort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("sandbox does not permit local port binding: %v", err)
		}
		t.Fatalf("failed to reserve test port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("failed to release test port: %v", err)
	}

	if err := ensureLocalPortAvailable(port); err != nil {
		t.Fatalf("expected released port %d to be available: %v", port, err)
	}
}
