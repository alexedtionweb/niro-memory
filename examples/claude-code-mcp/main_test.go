package main

import "testing"

func TestLocalAddress(t *testing.T) {
	for _, address := range []string{"localhost:3001", "127.0.0.1:3001", "[::1]:3001"} {
		if _, err := localAddress(address); err != nil {
			t.Errorf("%s: %v", address, err)
		}
	}
	for _, address := range []string{":3001", "0.0.0.0:3001", "[::]:3001", "192.168.1.1:3001", "example.com:3001", "3001"} {
		if _, err := localAddress(address); err == nil {
			t.Errorf("accepted nonlocal or malformed address %q", address)
		}
	}
}

func TestBackendAddress(t *testing.T) {
	for _, tc := range []struct {
		listen, backend string
		wantError       bool
	}{
		{"localhost:3001", "http://127.0.0.1:8080", false},
		{"localhost:8080", "http://127.0.0.1:8080", true},
		{"127.0.0.1:8080", "http://localhost:8080", true},
		{"[::1]:8080", "http://localhost:8080", true},
		{"localhost:80", "http://localhost", true},
		{"localhost:3001", "not-a-url", true},
		{"localhost:3001", "ftp://localhost:8080", true},
	} {
		t.Run(tc.listen+"/"+tc.backend, func(t *testing.T) {
			if err := validateBackendAddress(tc.listen, tc.backend); (err != nil) != tc.wantError {
				t.Fatalf("validateBackendAddress: %v; wantError=%v", err, tc.wantError)
			}
		})
	}
}
