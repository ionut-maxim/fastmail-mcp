package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestRunRefusesWithoutTokens(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name, fastmail, auth, addr, want string
	}{
		{"no fastmail token", "", "", "", "FASTMAIL_TOKEN"},
		{"http without auth token", "fm-token", "", ":0", "refusing to serve HTTP without MCP_AUTH_TOKEN"},
		{"http with blank auth token", "fm-token", "  \n", ":0", "refusing to serve HTTP without MCP_AUTH_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FASTMAIL_TOKEN", tc.fastmail)
			t.Setenv("MCP_AUTH_TOKEN", tc.auth)
			err := run(t.Context(), tc.addr, log)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
