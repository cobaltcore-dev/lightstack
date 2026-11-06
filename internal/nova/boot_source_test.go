package nova

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// A create must name what it boots from. Upstream refuses one that carries neither an imageRef
// nor a block device mapping with 400 "Missing imageRef attribute", before it looks anything up
// (nova/api/openstack/compute/servers.py, _image_from_req_data). Lightstack accepted it, in
// either mode, and left a server that could only end ERROR.
func TestCreateWithoutBootSourceIsRefused(t *testing.T) {
	requireDB(t)
	routers := []struct {
		mode string
		r    *gin.Engine
	}{
		{"stub", newNovaTestRouter()},
		{"real", newNoHypervisorService(t, nil)},
	}
	bodies := []struct{ name, fields string }{
		{"no imageRef", ``},
		{"empty imageRef", `,"imageRef":""`},
		{"empty mapping list", `,"block_device_mapping_v2":[]`},
	}
	for _, rt := range routers {
		for i, b := range bodies {
			t.Run(rt.mode+", "+b.name, func(t *testing.T) {
				name := "nosource-" + rt.mode + "-" + string(rune('a'+i))
				cleanupServerNamed(t, name)
				body := `{"server":{"name":"` + name + `","flavorRef":"m1.small"` + b.fields + `}}`

				code, resp := createServerAs(t, rt.r, seededProjectID, seededUserID, body)
				if code != http.StatusBadRequest || !strings.Contains(resp, "Missing imageRef attribute") {
					t.Fatalf("create: %d %s; want 400 \"Missing imageRef attribute\"", code, resp)
				}
				if n := instanceCount(t, name, seededProjectID); n != 0 {
					t.Errorf("%d server(s) created despite the refusal", n)
				}
			})
		}
	}
}
