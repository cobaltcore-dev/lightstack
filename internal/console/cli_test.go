package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Article II: text in, text out, JSON for structured data, errors to stderr,
// documented exit codes. These pin that protocol so the subcommand stays
// scriptable.

func TestCLIUsageErrorExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer

	for _, args := range [][]string{
		{},                     // no subcommand
		{"wat"},                // unknown subcommand
		{"list", "--nonsense"}, // unknown flag
	} {
		out.Reset()
		errOut.Reset()
		code := RunCLI(t.Context(), nil, args, &out, &errOut)
		if code != 2 {
			t.Errorf("RunCLI(%v) = %d; want 2 for a usage error", args, code)
		}
		if errOut.Len() == 0 {
			t.Errorf("RunCLI(%v) wrote nothing to stderr; usage errors must be diagnosable", args)
		}
		if out.Len() != 0 {
			t.Errorf("RunCLI(%v) wrote %q to stdout; usage errors belong on stderr", args, out.String())
		}
	}
}

func TestCLIHelpExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := RunCLI(t.Context(), nil, []string{"-h"}, &out, &errOut); code != 0 {
		t.Errorf("RunCLI(-h) = %d; want 0", code)
	}
}

// Only the commands that touch the store need one; the caller answers everything else
// without loading a config or connecting (and RunCLI handles those with a nil store).
func TestNeedsStore(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"list"}, true},
		{[]string{"purge", "--instance", "x"}, true},
		{nil, false},
		{[]string{"help"}, false},
		{[]string{"-h"}, false},
		{[]string{"--help"}, false},
		{[]string{"wat"}, false},
	} {
		if got := NeedsStore(tc.args); got != tc.want {
			t.Errorf("NeedsStore(%v) = %v; want %v", tc.args, got, tc.want)
		}
	}
}

func TestCLIListText(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	inst, proj := seedInstance(t, ctx)

	tok, err := s.Issue(ctx, inst, proj, testSocket, testDomainID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := RunCLI(ctx, s, []string{"list"}, &out, &errOut); code != 0 {
		t.Fatalf("list = %d, stderr=%q", code, errOut.String())
	}

	got := out.String()
	if !strings.Contains(got, inst) {
		t.Errorf("list output missing instance id %s:\n%s", inst, got)
	}
	if !strings.Contains(got, testSocket) {
		t.Errorf("list output missing the target socket:\n%s", got)
	}
	// The run the token was issued for: what tells a restarted guest from this one.
	if !strings.Contains(got, "DOMAIN") || !strings.Contains(got, fmt.Sprint(testDomainID)) {
		t.Errorf("list output missing the domain ID column:\n%s", got)
	}
	// The whole point of redaction is that a shoulder-surfed listing is not a
	// usable credential (FR-028).
	if strings.Contains(got, tok) {
		t.Errorf("list printed the full token %q — it must be redacted", tok)
	}
}

func TestCLIListJSON(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	inst, proj := seedInstance(t, ctx)

	if _, err := s.Issue(ctx, inst, proj, testSocket, testDomainID); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := RunCLI(ctx, s, []string{"list", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("list --json = %d, stderr=%q", code, errOut.String())
	}

	var got []struct {
		Token      string    `json:"token"`
		InstanceID string    `json:"instance_id"`
		Socket     string    `json:"socket"`
		DomainID   int32     `json:"domain_id"`
		ExpiresAt  time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	var found bool
	for _, r := range got {
		if r.InstanceID == inst && r.Socket == testSocket {
			found = true
			if r.DomainID != testDomainID {
				t.Errorf("json domain_id = %d; want %d", r.DomainID, testDomainID)
			}
			if strings.Contains(r.Token, " ") || len(r.Token) > 12 {
				t.Errorf("json token %q should be redacted too", r.Token)
			}
		}
	}
	if !found {
		t.Errorf("issued token missing from JSON output:\n%s", out.String())
	}
}

func TestCLIPurgeRemovesExpired(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	inst, proj := seedInstance(t, ctx)

	tok, err := s.Issue(ctx, inst, proj, testSocket, testDomainID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		"UPDATE console_auth_tokens SET expires_at = $2 WHERE token = $1", tok, time.Now().Add(-time.Minute),
	); err != nil {
		t.Fatalf("backdating: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := RunCLI(ctx, s, []string{"purge"}, &out, &errOut); code != 0 {
		t.Fatalf("purge = %d, stderr=%q", code, errOut.String())
	}
	if _, err := s.Resolve(ctx, tok); err == nil {
		t.Error("expired token still resolves after purge")
	}
}

func TestCLIPurgeByInstance(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	inst, proj := seedInstance(t, ctx)

	tok, err := s.Issue(ctx, inst, proj, testSocket, testDomainID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := RunCLI(ctx, s, []string{"purge", "--instance", inst}, &out, &errOut); code != 0 {
		t.Fatalf("purge --instance = %d, stderr=%q", code, errOut.String())
	}
	// An operator revoking a live console must actually revoke it, not just the
	// expired ones.
	if _, err := s.Resolve(ctx, tok); err == nil {
		t.Error("live token still resolves after an explicit per-instance purge")
	}
}
