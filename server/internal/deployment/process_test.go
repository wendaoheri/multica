package deployment

import "testing"

func TestParseProcessRole(t *testing.T) {
	tests := []struct {
		raw  string
		want ProcessRole
		err  bool
	}{
		{"", RoleAll, false},
		{"ALL", RoleAll, false},
		{" web ", RoleWeb, false},
		{"worker", RoleWorker, false},
		{"api", "", true},
	}
	for _, tt := range tests {
		got, err := ParseProcessRole(tt.raw)
		if (err != nil) != tt.err || got != tt.want {
			t.Fatalf("ParseProcessRole(%q) = %q, %v; want %q, err=%v", tt.raw, got, err, tt.want, tt.err)
		}
	}
}

func TestValidateLoopbackAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9091", "[::1]:9091", "localhost:9091"} {
		if err := ValidateLoopbackAddr(addr); err != nil {
			t.Fatalf("ValidateLoopbackAddr(%q): %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:9091", ":9091", "10.0.0.1:9091", "bad"} {
		if err := ValidateLoopbackAddr(addr); err == nil {
			t.Fatalf("ValidateLoopbackAddr(%q) unexpectedly succeeded", addr)
		}
	}
}
