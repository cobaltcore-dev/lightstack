package common

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestVisibleImage pins upstream Glance's rule for reading one image
// (glance/db/utils.py is_image_visible), which decides whether a server or a volume may
// be made from it and whether Glance shows it: an administrator sees every image; anyone
// sees a public or community image, or one with no owner; the owner sees its own; and a
// project a shared image was shared with sees it, whatever the membership's status. An
// unknown or malformed id is visible to no one. A visible image's status comes back with
// it, because only an active image can be booted.
func TestVisibleImage(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	caller := newTestProject(t, "imgvis-caller")
	owner := newTestProject(t, "imgvis-owner")

	image := func(name, visibility, project string) string {
		t.Helper()
		var projectID any
		if project != "" {
			projectID = project
		}
		status := "active"
		if name == "own-queued" {
			status = "queued"
		}
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO images (name, project_id, status, visibility) VALUES ($1, $2, $3, $4) RETURNING id::text`,
			name, projectID, status, visibility).Scan(&id); err != nil {
			t.Fatalf("seeding image %s: %v", name, err)
		}
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM image_members WHERE image_id = $1`, id)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM images WHERE id = $1`, id)
		})
		return id
	}
	member := func(imageID, memberID, status string) {
		t.Helper()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO image_members (image_id, member_id, status) VALUES ($1, $2, $3)`,
			imageID, memberID, status); err != nil {
			t.Fatalf("adding member: %v", err)
		}
	}

	ownPrivate := image("own-private", "private", caller)
	foreignPrivate := image("foreign-private", "private", owner)
	foreignPublic := image("foreign-public", "public", owner)
	foreignCommunity := image("foreign-community", "community", owner)
	ownerless := image("ownerless", "private", "")
	sharedPending := image("shared-pending", "shared", owner)
	member(sharedPending, caller, "pending")
	sharedDashFree := image("shared-dash-free", "shared", owner)
	member(sharedDashFree, strings.ReplaceAll(caller, "-", ""), "accepted")
	sharedOther := image("shared-with-someone-else", "shared", owner)
	member(sharedOther, uuid.New().String(), "accepted")
	privateWithMember := image("private-with-a-member", "private", owner)
	member(privateWithMember, caller, "accepted")
	ownQueued := image("own-queued", "private", caller)

	for _, tc := range []struct {
		name    string
		imageID string
		admin   bool
		want    bool
	}{
		{"the caller's own image with no data yet", ownQueued, false, true},
		{"the caller's own private image", ownPrivate, false, true},
		{"another project's private image", foreignPrivate, false, false},
		{"another project's private image, as an administrator", foreignPrivate, true, true},
		{"a public image", foreignPublic, false, true},
		{"a community image", foreignCommunity, false, true},
		{"an image with no owner", ownerless, false, true},
		{"a shared image, membership pending", sharedPending, false, true},
		{"a shared image, member id without dashes", sharedDashFree, false, true},
		{"a shared image shared with another project", sharedOther, false, false},
		{"a private image with a stray membership", privateWithMember, false, false},
		{"an unknown id", uuid.New().String(), false, false},
		{"an unknown id, as an administrator", uuid.New().String(), true, false},
		{"a malformed id", "not-a-uuid", true, false},
		{"an empty id", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, got, err := VisibleImage(ctx, testPool, tc.imageID, caller, tc.admin)
			if err != nil {
				t.Fatalf("VisibleImage: %v", err)
			}
			if got != tc.want {
				t.Errorf("VisibleImage found = %v, want %v", got, tc.want)
			}
			wantStatus := ""
			if tc.want {
				wantStatus = "active"
				if tc.imageID == ownQueued {
					wantStatus = "queued"
				}
			}
			if status != wantStatus {
				t.Errorf("VisibleImage status = %q, want %q", status, wantStatus)
			}
		})
	}
}

// newTestProject creates a project for the test and removes it afterwards.
func newTestProject(t *testing.T, prefix string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO projects (name, description) VALUES ($1, 'common test fixture') RETURNING id::text`,
		prefix+"-"+uuid.New().String()[:8]).Scan(&id); err != nil {
		t.Fatalf("creating project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	return id
}
