package sshsrv

import "testing"

func TestCheckLoopbackAddr(t *testing.T) {
	for _, a := range []string{"127.0.0.1:9200", "127.0.0.5:9200", "[::1]:9200", "localhost:9200", "127.0.0.1:0"} {
		if err := CheckLoopbackAddr(a); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	for _, a := range []string{"0.0.0.0:9200", ":9200", "10.50.0.1:9200", "[::]:9200", "example.com:9200", "127.0.0.1", "127.0.0.1:"} {
		if err := CheckLoopbackAddr(a); err == nil {
			t.Errorf("%s: want an error", a)
		}
	}
}

func TestServeAdminRefusesNonLoopback(t *testing.T) {
	s := &Server{}
	if err := s.ServeAdmin("10.50.0.1:9200", nil); err == nil || s.adminAddr != "" {
		t.Fatalf("non-loopback admin address accepted: %v", err)
	}
}
