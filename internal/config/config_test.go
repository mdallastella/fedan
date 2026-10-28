package config

import "testing"

func TestAddrWithoutPassword(t *testing.T) {
	for addr, ok := range map[string]bool{
		"":               true, // defaults to 127.0.0.1:8080
		"127.0.0.1:9000": true,
		"localhost:80":   true,
		"[::1]:8080":     true,
		":8080":          false,
		"0.0.0.0:8080":   false,
		"192.168.1.2:80": false,
	} {
		t.Setenv("FEDAN_ADDR", addr)
		t.Setenv("FEDAN_PASSWORD", "")
		t.Setenv("FEDAN_PASSWORD_HASH", "")
		c, err := Load()
		if (err == nil) != ok {
			t.Errorf("addr %q without password: err = %v", addr, err)
		}
		if addr == "" && c.Addr != "127.0.0.1:8080" {
			t.Errorf("default addr without password = %s", c.Addr)
		}
		t.Setenv("FEDAN_PASSWORD", "secret")
		if _, err := Load(); err != nil {
			t.Errorf("addr %q with password: %v", addr, err)
		}
	}
}

func TestPasswordHash(t *testing.T) {
	t.Setenv("FEDAN_PASSWORD_HASH", "change-me")
	if _, err := Load(); err == nil {
		t.Error("malformed hash accepted")
	}
	t.Setenv("FEDAN_PASSWORD_HASH", "$2a$10$Ju6emXL8pWUeB8pEk6M3tu5cZcoYE2KOpnYsG5aMKtSXRc4qyl.LG")
	if _, err := Load(); err != nil {
		t.Errorf("valid hash rejected: %v", err)
	}
}
