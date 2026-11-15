package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCommands(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		out  string // expected in stdout or stderr
	}{
		{nil, 3, "usage: refraktal"},
		{[]string{"--help"}, 0, "doctor          check harnesses"},
		{[]string{"run", "fix it"}, 3, "not implemented yet"},
		{[]string{"bogus"}, 3, `unknown command "bogus"`},
		{[]string{"doctor", "--bogus"}, 3, "flag provided but not defined"},
	} {
		var out, errb bytes.Buffer
		code := run(context.Background(), tc.args, &out, &errb)
		if code != tc.code || !strings.Contains(out.String()+errb.String(), tc.out) {
			t.Errorf("%v: code %d, output %q", tc.args, code, out.String()+errb.String())
		}
	}
}
