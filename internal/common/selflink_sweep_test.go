package common

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// SPEC-025 T015 (FR-007, FR-007a, SC-003): no emitter anywhere in internal/ may build
// a self-link by hand.
//
// SC-003 claims "every self-link the product emits is an absolute URL — 0 responses
// carry a scheme-less or empty link". Sampling a few endpoints cannot support the word
// "every", and the analyze pass flagged exactly that: the claim was not verifiable by
// the tasks that were meant to verify it. This sweeps the source instead, so a new
// hand-built link fails on the day it is written rather than on the day someone
// remembers to extend a response-level test.
//
// It is a source check, not a behavioral one — the response-level counterpart is
// TestNovaSelfLinksAreAbsolute_Contract. Both exist because they fail differently: this
// one names the file and line, that one proves what reaches the wire.
func TestNoHandBuiltSelfLinks(t *testing.T) {
	root := repoRelative(t, "internal")

	// Each pattern is a way a self-link has actually been built wrong in this
	// codebase, not a hypothetical.
	patterns := []struct {
		name string
		re   *regexp.Regexp
		why  string
	}{
		{
			name: "scheme-less concatenation of the request host",
			re:   regexp.MustCompile(`c\.Request\.Host\s*\+`),
			why:  `produces "host/v3/x" with no scheme; use common.BaseURL`,
		},
		{
			name: "hardcoded scheme prefixed to the request host",
			re:   regexp.MustCompile(`"https?://"\s*\+\s*c\.Request\.Host`),
			why:  `ignores LIGHTSTACK_ENDPOINT_SCHEME and TLS termination; use common.BaseURL`,
		},
		{
			name: "hardcoded compose-file hostname in a URL",
			re:   regexp.MustCompile(`"https?://lightstack[:/]`),
			why:  `resolves only inside the compose network; use common.BaseURL`,
		},
		{
			name: "relative href",
			re:   regexp.MustCompile(`"href":\s*"/`),
			why:  `a client resolving a version document has nothing to resolve it against; use common.BaseURL`,
		},
	}

	// Known-good exceptions, each justified. Keep this list short: an entry here is a
	// claim that the match is not a self-link at all.
	allowed := map[string]string{
		// The metadata service's "links" are cloud-init network-data interface
		// descriptors (tap devices), not HTTP self-links. Different concept, same word.
		filepath.Join("internal", "metadata", "service.go"): "cloud-init interface links, not self-links",

		// substituteURLTemplates REPAIRS stored catalog rows (seed data pins
		// "http://lightstack:<port>"); it does not emit a self-link. Excluded on that basis,
		// not because it is beyond reproach: it resolves the advertised host from
		// LIGHTSTACK_ENDPOINT_HOST alone and never consults the request, so the catalog does
		// not follow FR-002's precedence the way self-links now do. That is a
		// deployment-wide surface owned by SPEC-007, outside this feature's FRs, and
		// is recorded as a follow-up rather than changed here.
		filepath.Join("internal", "keystone", "auth.go"): "repairs stored catalog URLs; not a self-link emitter (follow-up: catalog ignores the request host)",
	}

	var findings []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := relFromRepo(t, path)
		if reason, ok := allowed[rel]; ok {
			t.Logf("skipping %s: %s", rel, reason)
			return nil
		}

		data, readErr := os.ReadFile(path) //nolint:gosec // test-only walk of the repo's own source
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(data), "\n") {
			code := stripComment(line)
			if strings.TrimSpace(code) == "" {
				continue
			}
			for _, p := range patterns {
				if p.re.MatchString(code) {
					findings = append(findings,
						rel+":"+itoa(i+1)+"  ["+p.name+"] "+p.why+"\n      "+strings.TrimSpace(line))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/: %v", err)
	}

	if len(findings) > 0 {
		t.Errorf("%d hand-built self-link(s) — every self-link must come from common.BaseURL:\n  %s",
			len(findings), strings.Join(findings, "\n  "))
	}
}

// stripComment removes a trailing // comment so the sweep matches code, not prose.
// A comment that quotes the very URL shape being outlawed — as several of the fixes
// for this feature do, to explain what they replaced — must not read as a violation.
// Quotes are tracked so a "//" inside a string literal is not mistaken for a comment.
func stripComment(line string) string {
	inString := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			// A quote escaped with a backslash does not open or close a literal.
			if i == 0 || line[i-1] != '\\' {
				inString = !inString
			}
		case '/':
			if !inString && i+1 < len(line) && line[i+1] == '/' {
				return line[:i]
			}
		}
	}
	return line
}

// repoRelative resolves a path relative to the repository root from the package's own
// directory, so the test works regardless of where `go test` is invoked.
func repoRelative(t *testing.T, sub string) string {
	t.Helper()
	// This file lives in internal/common, so the root is two levels up.
	return filepath.Join("..", "..", sub)
}

// relFromRepo renders a walked path as a repo-relative one for readable failures.
func relFromRepo(t *testing.T, path string) string {
	t.Helper()
	cleaned := filepath.Clean(path)
	cleaned = strings.TrimPrefix(cleaned, ".."+string(filepath.Separator))
	cleaned = strings.TrimPrefix(cleaned, ".."+string(filepath.Separator))
	return cleaned
}

// itoa avoids pulling strconv in for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
