package common

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// PaginationParams holds the parsed pagination query parameters from a request.
type PaginationParams struct {
	Limit   int
	Offset  int
	Marker  string
	SortKey string
	SortDir string
}

// validSortKeys is the set of column names that callers may sort by.
// This allowlist prevents SQL injection through the sort_key parameter.
var validSortKeys = map[string]bool{
	"created_at": true,
	"updated_at": true,
	"name":       true,
	"id":         true,
	"status":     true,
	"size":       true,
	"size_gb":    true,
}

// ParsePagination extracts limit, offset, marker, sort_key, and sort_dir from
// the request query string. defaultLimit is used when the caller does not
// supply a limit (or supplies an invalid one).
func ParsePagination(c *gin.Context, defaultLimit int) PaginationParams {
	sortKey := c.DefaultQuery("sort_key", "created_at")
	if !validSortKeys[sortKey] {
		sortKey = "created_at"
	}

	p := PaginationParams{
		Limit:   defaultLimit,
		Marker:  c.Query("marker"),
		SortKey: sortKey,
		SortDir: c.DefaultQuery("sort_dir", "desc"),
	}

	if limitStr := c.Query("limit"); limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 {
			p.Limit = v
		}
	}

	if offsetStr := c.Query("offset"); offsetStr != "" {
		if v, err := strconv.Atoi(offsetStr); err == nil && v >= 0 {
			p.Offset = v
		}
	}

	if p.SortDir != "asc" && p.SortDir != "desc" {
		p.SortDir = "desc"
	}

	return p
}
