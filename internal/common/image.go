package common

import (
	"context"
	"errors"
	"fmt"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/google/uuid"
)

// ImageVisibleSQL is upstream Glance's rule for reading one image
// (glance/db/utils.py is_image_visible) as a SQL condition on the images row aliased
// alias (SPEC-020 FR-026):
//
//   - an administrator reads every image;
//   - anyone reads a public or community image, or one with no owner;
//   - the owning project reads its own;
//   - a project a shared image was shared with reads it, whatever the membership's status.
//
// projectParam and adminParam are the placeholder numbers of the caller's project id, as
// CallerProjectID renders it, and of the administrator flag. Glance's show and download
// use it, and so does every API that makes a server or a volume from an image, so they
// cannot disagree. A member id is compared without dashes: a project id reaches the
// image-sharing API in either form (rgw_compat_tenant_ids puts the dash-free one in
// tokens).
func ImageVisibleSQL(alias string, projectParam, adminParam int) string {
	return fmt.Sprintf(`($%[3]d
		OR %[1]s.project_id IS NULL
		OR %[1]s.visibility IN ('public', 'community')
		OR %[1]s.project_id::text = $%[2]d
		OR (%[1]s.visibility = 'shared' AND $%[2]d <> '' AND EXISTS (
			SELECT 1 FROM image_members m
			WHERE m.image_id = %[1]s.id
			  AND replace(lower(m.member_id), '-', '') = replace($%[2]d, '-', ''))))`,
		alias, projectParam, adminParam)
}

// CallerProjectID renders a project id the way ImageVisibleSQL compares it: the canonical
// UUID form, or "" when it is not a UUID, which matches no owner and no membership.
func CallerProjectID(projectID string) string {
	if p, err := uuid.Parse(projectID); err == nil {
		return p.String()
	}
	return ""
}

// VisibleImage looks up an image the caller may read, by ImageVisibleSQL, and reports its
// status ("active" once its data is uploaded): the check every API that makes a server or a
// volume from an image applies before using it. Upstream does it by asking Glance with the
// caller's token; Lightstack shares the database.
//
// found is false for an image the caller cannot read and for an id that names no image or
// is not a UUID, so callers answer "not found" for all of them and a refusal does not tell
// which.
func VisibleImage(ctx context.Context, pool database.DB, imageID, projectID string, admin bool) (status string, found bool, err error) {
	id, perr := uuid.Parse(imageID)
	if perr != nil {
		return "", false, nil
	}
	err = pool.QueryRow(ctx,
		`SELECT i.status FROM images i WHERE i.id = $1 AND `+ImageVisibleSQL("i", 2, 3),
		id.String(), CallerProjectID(projectID), admin,
	).Scan(&status)
	if errors.Is(err, database.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("checking image %s: %w", id, err)
	}
	return status, true, nil
}
