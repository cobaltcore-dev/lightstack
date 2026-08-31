package nova

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// SPEC-025 FR-041 (cloud-storage#547 B7): no query may exclude a server on the basis of
// a status the product never assigns.
//
// This is a SOURCE check, deliberately, because no behavioural test can catch it. The
// usage query filtered `AND i.status != 'DELETED'` and nothing has ever written that
// status — every delete path is a hard DELETE FROM instances — so the clause excluded
// nothing and every assertion about reported usage passed with it in place. Confirmed by
// mutation during T125: restoring the filter leaves the usage contract test green.
//
// That is exactly why it was worth removing. It was a latent under-count, not a current
// one: the moment anything began writing that status, the limits view would silently stop
// counting those servers while the quota views kept counting them. A test that only looks
// at behaviour would go on passing until then.
func TestNoFiltersOnStatusesNothingWrites(t *testing.T) {
	// Statuses that appear in comparisons but are never assigned anywhere in the
	// product. Each must be justified before being added here.
	deadStatuses := []string{"DELETED"}

	// A status is "written" if it appears in an INSERT or UPDATE. Read comparisons are
	// what this test hunts.
	writes := regexp.MustCompile(`(?i)(SET\s+status\s*=|INSERT\s+INTO\s+instances)`)
	reads := regexp.MustCompile(`status\s*(!=|=|<>)\s*'([A-Z_]+)'`)

	root := filepath.Join("..", "..", "internal")
	var findings []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path) //nolint:gosec // test-only walk of the repo's own source
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(data), "\n") {
			code := stripLineComment(line)
			if writes.MatchString(code) {
				continue
			}
			for _, m := range reads.FindAllStringSubmatch(code, -1) {
				for _, dead := range deadStatuses {
					if m[2] == dead {
						findings = append(findings, strings.TrimSpace(path)+":"+
							itoaLocal(i+1)+"  compares status against "+dead+
							", which nothing writes:\n      "+strings.TrimSpace(line))
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/: %v", err)
	}

	if len(findings) > 0 {
		t.Errorf("%d comparison(s) against a status the product never assigns — "+
			"such a filter excludes nothing today and silently under-counts the moment it does:\n  %s",
			len(findings), strings.Join(findings, "\n  "))
	}
}

// stripLineComment removes a trailing // comment so prose explaining a removed filter is
// not mistaken for the filter itself.
func stripLineComment(line string) string {
	inString := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"', '`':
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

func itoaLocal(n int) string {
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
