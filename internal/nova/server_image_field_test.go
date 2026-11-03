package nova

import (
	"database/sql"
	"testing"

	"github.com/gin-gonic/gin"
)

// A server response must always carry an "image" member: an {id} object for an
// image-booted server, and "" for a boot-from-volume server. Horizon's instance detail
// view reads server.image unconditionally, so omitting it (the old single-server bug)
// made novaclient raise "AttributeError: image".
func TestServerImageField(t *testing.T) {
	img := serverImageField(sql.NullString{String: "img-1", Valid: true})
	m, ok := img.(gin.H)
	if !ok || m["id"] != "img-1" {
		t.Errorf("image-boot: serverImageField = %#v, want gin.H{id: img-1}", img)
	}
	if got := serverImageField(sql.NullString{}); got != "" {
		t.Errorf("boot-from-volume: serverImageField = %#v, want \"\" (present, empty)", got)
	}
}
