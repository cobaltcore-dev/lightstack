package common

import (
	"net/http"
	"testing"
)

func TestNewIdentityNotFoundError(t *testing.T) {
	e := NewIdentityNotFoundError("project", "not-a-uuid")
	if e.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", e.StatusCode)
	}
	if e.Code != "itemNotFound" {
		t.Errorf("Code = %q, want itemNotFound", e.Code)
	}
	if want := "Could not find project: not-a-uuid."; e.Message != want {
		t.Errorf("Message = %q, want %q", e.Message, want)
	}
	if e.Details != "" {
		t.Errorf("Details = %q, want empty", e.Details)
	}
}

// TestNewRoleAssignmentNotFoundError pins the message to Keystone's
// RoleAssignmentNotFound.message_format (keystone/exception.py). A role assignment
// is a (role, actor, target) triple, so it deliberately does NOT use the
// "Could not find <resource>: <id>." form.
func TestNewRoleAssignmentNotFoundError(t *testing.T) {
	e := NewRoleAssignmentNotFoundError("r-1", "u-2", "p-3")
	if e.StatusCode != http.StatusNotFound || e.Code != "itemNotFound" {
		t.Fatalf("status/code = %d/%q, want 404/itemNotFound", e.StatusCode, e.Code)
	}
	want := "Could not find role assignment with role: r-1, user or group: u-2, " +
		"project, domain, or system: p-3."
	if e.Message != want {
		t.Errorf("Message = %q, want %q", e.Message, want)
	}
}

// NewProjectNotFoundError must keep its verbatim message after being refactored
// to delegate to NewIdentityNotFoundError.
func TestNewProjectNotFoundErrorDelegates(t *testing.T) {
	e := NewProjectNotFoundError("00000000-0000-0000-0000-0000000000ff")
	if e.StatusCode != http.StatusNotFound || e.Code != "itemNotFound" {
		t.Fatalf("status/code = %d/%q, want 404/itemNotFound", e.StatusCode, e.Code)
	}
	if want := "Could not find project: 00000000-0000-0000-0000-0000000000ff."; e.Message != want {
		t.Errorf("Message = %q, want %q", e.Message, want)
	}
}
