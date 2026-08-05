package console

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/database"
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

func TestCLIListText(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	inst, proj := seedInstance(t, ctx)

	tok, err := s.Issue(ctx, inst, proj, "127.0.0.1", 5901)
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
	if !strings.Contains(got, "127.0.0.1:5901") {
		t.Errorf("list output missing the target endpoint:\n%s", got)
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

	if _, err := s.Issue(ctx, inst, proj, "127.0.0.1", 5902); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := RunCLI(ctx, s, []string{"list", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("list --json = %d, stderr=%q", code, errOut.String())
	}

	var got []struct {
		Token      string    `json:"token"`
		InstanceID string    `json:"instance_id"`
		Host       string    `json:"host"`
		Port       int       `json:"port"`
		ExpiresAt  time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	var found bool
	for _, r := range got {
		if r.InstanceID == inst && r.Port == 5902 {
			found = true
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

	tok, err := s.Issue(ctx, inst, proj, "127.0.0.1", 5903)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := database.DB.Exec(ctx,
		"UPDATE console_auth_tokens SET expires_at = now() - interval '1 minute' WHERE token = $1", tok,
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

	tok, err := s.Issue(ctx, inst, proj, "127.0.0.1", 5904)
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
