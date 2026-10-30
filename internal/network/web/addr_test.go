package web

import (
	"net"
	"testing"
)

func TestValidateBindAddr(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:7777", true},
		{"[::1]:7777", true},
		{"100.101.102.103:7777", true}, // tailnet CGNAT range
		{"0.0.0.0:7777", false},
		{"[::]:7777", false},
		{"192.168.1.5:7777", false},
		{"8.8.8.8:7777", false},
		{":7777", false},          // empty host == every interface
		{"localhost:7777", false}, // hostname, not a literal IP
		{"127.0.0.1:abc", false},  // bad port
		{"127.0.0.1", false},      // missing port
	}
	for _, c := range cases {
		ip, err := validateBindAddr(c.addr)
		if c.ok && err != nil {
			t.Errorf("validateBindAddr(%q) = err %v, want ok", c.addr, err)
		}
		if !c.ok && err == nil {
			t.Errorf("validateBindAddr(%q) = ip %v, want error", c.addr, ip)
		}
	}
}

func TestIsCGNAT(t *testing.T) {
	cases := []struct {
		ip string
		ok bool
	}{
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"100.63.255.255", false},
		{"100.128.0.0", false},
		{"10.0.0.1", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("net.ParseIP(%q) = nil", c.ip)
		}
		if got := isCGNAT(ip); got != c.ok {
			t.Errorf("isCGNAT(%q) = %v, want %v", c.ip, got, c.ok)
		}
	}
}
