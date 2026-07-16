package keystone

import (
	"errors"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// notFoundOr500 maps the error from an identity by-id statement to a response: a
// real not-found (pgx.ErrNoRows) becomes Keystone's verbatim 404
// ("Could not find <resource>: <id>."), while any other error is a genuine
// backend fault and surfaces as 500 — never masked as 404 (cloud-storage#515,
// FR-007/FR-010). It replaces both common.HandleDatabaseError call sites and the
// pre-existing `if err != nil -> NotFound` masking on identity handlers. It serves
// plain SELECTs and the `UPDATE ... RETURNING` used by the PATCH handlers alike,
// since both report "no such id" as ErrNoRows.
func notFoundOr500(c *gin.Context, err error, resource, id string) {
	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewIdentityNotFoundError(resource, id))
		return
	}
	log.Error().Err(err).Str("resource", resource).Str("id", id).Msg("identity query failed")
	common.SendError(c, common.NewDatabaseError("query", resource, err))
}

// existsOr404 handles the EXISTS-style lookup (SELECT EXISTS(...) -> bool): a
// genuine query error is a 500 (never masked as not-found), and a well-formed id
// that matched no row is the Keystone-verbatim 404. Returns true when it has
// written a response and the caller should return. This replaces the pre-existing
// `if err != nil || !exists { NotFound }` masking (cloud-storage#515 FR-010).
func existsOr404(c *gin.Context, err error, exists bool, resource, id string) bool {
	if err != nil {
		log.Error().Err(err).Str("resource", resource).Str("id", id).Msg("identity existence check failed")
		common.SendError(c, common.NewDatabaseError("query", resource, err))
		return true
	}
	if !exists {
		common.SendError(c, common.NewIdentityNotFoundError(resource, id))
		return true
	}
	return false
}
