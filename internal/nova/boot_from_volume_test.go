package nova

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// parseServerReq decodes a server-create body the same way gin does in real
// requests — with UseNumber() (binding.EnableDecoderUseNumber), so numeric
// JSON fields arrive as json.Number, not float64. This is exactly the path
// that broke boot_index parsing once (SPEC-009 WS-5).
func parseServerReq(t *testing.T, body string) *CreateServerRequest {
	t.Helper()
	var req CreateServerRequest
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &req
}

func TestBootVolumeID(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantID  string
		wantDOT bool
	}{
		{
			name:   "boot-from-volume, numeric boot_index 0 (json.Number — the regression case)",
			body:   `{"server":{"name":"v","block_device_mapping_v2":[{"uuid":"vol-1","boot_index":0,"source_type":"volume","destination_type":"volume"}]}}`,
			wantID: "vol-1",
		},
		{
			name:    "delete_on_termination true is reported",
			body:    `{"server":{"name":"v","block_device_mapping_v2":[{"uuid":"vol-2","boot_index":0,"source_type":"volume","delete_on_termination":true}]}}`,
			wantID:  "vol-2",
			wantDOT: true,
		},
		{
			name:   "string boot_index \"0\"",
			body:   `{"server":{"name":"v","block_device_mapping_v2":[{"uuid":"vol-3","boot_index":"0","source_type":"volume"}]}}`,
			wantID: "vol-3",
		},
		{
			name:   "missing boot_index (nil) treated as root",
			body:   `{"server":{"name":"v","block_device_mapping_v2":[{"uuid":"vol-4","source_type":"volume"}]}}`,
			wantID: "vol-4",
		},
		{
			name:   "image boot, no block device mapping",
			body:   `{"server":{"name":"v","imageRef":"img-1"}}`,
			wantID: "",
		},
		{
			name:   "non-root BDM (boot_index 1) is not the boot volume",
			body:   `{"server":{"name":"v","block_device_mapping_v2":[{"uuid":"vol-5","boot_index":1,"source_type":"volume"}]}}`,
			wantID: "",
		},
		{
			name:   "source_type image is ignored (not a volume)",
			body:   `{"server":{"name":"v","block_device_mapping_v2":[{"uuid":"img-2","boot_index":0,"source_type":"image"}]}}`,
			wantID: "",
		},
		{
			name:   "data volume at boot_index 1 + image root: image boot wins",
			body:   `{"server":{"name":"v","imageRef":"img-3","block_device_mapping_v2":[{"uuid":"vol-6","boot_index":1,"source_type":"volume"}]}}`,
			wantID: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := parseServerReq(t, tt.body)
			gotID, gotDOT := bootVolumeID(req)
			if gotID != tt.wantID || gotDOT != tt.wantDOT {
				t.Errorf("bootVolumeID() = (%q, %v), want (%q, %v)", gotID, gotDOT, tt.wantID, tt.wantDOT)
			}
		})
	}
}

// TestBootImageRefFromBDM covers N4 (SPEC-020 / cloud-storage#533): Horizon's Launch
// Instance wizard sends the image inside block_device_mapping_v2, with imageRef empty,
// whenever "Create New Volume" is on — its upstream default. Lightstack ignored that entry and
// then rejected the empty imageRef ("invalid UUID length: 0"), so every dashboard launch
// on the default setting produced an ERROR instance.
func TestBootImageRefFromBDM(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "Horizon's wizard default: image via BDM, string boot_index, empty imageRef",
			body: `{"server":{"name":"v","imageRef":"","block_device_mapping_v2":[{"uuid":"img-1","source_type":"image","destination_type":"volume","delete_on_termination":true,"boot_index":"0"}]}}`,
			want: "img-1",
		},
		{
			name: "numeric boot_index 0 (json.Number)",
			body: `{"server":{"name":"v","block_device_mapping_v2":[{"uuid":"img-2","source_type":"image","boot_index":0}]}}`,
			want: "img-2",
		},
		{
			name: "missing boot_index treated as root",
			body: `{"server":{"name":"v","block_device_mapping_v2":[{"uuid":"img-3","source_type":"image"}]}}`,
			want: "img-3",
		},
		{
			name: "non-root image BDM (boot_index 1) is not the boot image",
			body: `{"server":{"name":"v","block_device_mapping_v2":[{"uuid":"img-4","source_type":"image","boot_index":1}]}}`,
			want: "",
		},
		{
			name: "source_type volume is not an image source",
			body: `{"server":{"name":"v","block_device_mapping_v2":[{"uuid":"vol-1","source_type":"volume","boot_index":0}]}}`,
			want: "",
		},
		{
			name: "no block device mapping at all",
			body: `{"server":{"name":"v","imageRef":"img-5"}}`,
			want: "",
		},
		{
			name: "empty uuid is not a source",
			body: `{"server":{"name":"v","block_device_mapping_v2":[{"uuid":"","source_type":"image","boot_index":0}]}}`,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bootImageRefFromBDM(parseServerReq(t, tt.body)); got != tt.want {
				t.Errorf("bootImageRefFromBDM() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBootSourcePrecedence pins the resolution order CreateServer applies, because the
// two BDM readers overlap: an explicit imageRef wins over any BDM, and a real boot
// volume wins over an image BDM (so adopting the image entry cannot hijack the
// SPEC-009 WS-5 boot-from-volume path).
//
// This drives the request through the router and asserts the **persisted** image, not a
// re-implementation of the normalization. An earlier version of this test copied that
// logic into itself, which meant the production block could be deleted outright with the
// whole suite still green — no coverage at all on the flagship SC-001 path.
func TestBootSourcePrecedence(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)

	const (
		explicitImg = "aaaaaaaa-0000-0000-0000-00000000aaaa"
		bdmImg      = "bbbbbbbb-0000-0000-0000-00000000bbbb"
		rootVolume  = "cccccccc-0000-0000-0000-00000000cccc"
		flavor      = "00000000-0000-0000-0000-000000000011"
	)

	tests := []struct {
		name string
		// bdmAndImage is spliced into the server object: the imageRef plus the BDM list.
		bdmAndImage string
		wantImageID string // what must land in instances.image_id
	}{
		{
			name:        "explicit imageRef wins over an image BDM",
			bdmAndImage: `"imageRef":"` + explicitImg + `","block_device_mapping_v2":[{"uuid":"` + bdmImg + `","source_type":"image","boot_index":"0"}]`,
			wantImageID: explicitImg,
		},
		{
			name:        "real boot volume wins: no image is adopted",
			bdmAndImage: `"imageRef":"","block_device_mapping_v2":[{"uuid":"` + rootVolume + `","source_type":"volume","boot_index":"0"},{"uuid":"` + bdmImg + `","source_type":"image","boot_index":"0"}]`,
			wantImageID: "",
		},
		{
			// Horizon's launch wizard, verbatim: "Create New Volume" is on by default,
			// so it sends the image inside the BDM and leaves imageRef empty.
			name:        "image BDM only: adopted as the image source",
			bdmAndImage: `"imageRef":"","block_device_mapping_v2":[{"boot_index":"0","uuid":"` + bdmImg + `","source_type":"image","destination_type":"volume","volume_size":1,"delete_on_termination":true}]`,
			wantImageID: bdmImg,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"server":{"name":"bootsrc","flavorRef":%q,%s}}`, flavor, tt.bdmAndImage)

			code, resp := doReq(r, http.MethodPost, "/v2.1/servers", body, hdr)
			if code != http.StatusAccepted {
				t.Fatalf("create: code=%d body=%s; want 202", code, resp)
			}
			id := createdServerID(t, resp)
			// context.Background(), not t.Context(): the test context is already
			// cancelled by the time cleanups run.
			t.Cleanup(func() {
				if _, err := database.DB.Exec(context.Background(), "DELETE FROM instances WHERE id = $1", id); err != nil {
					t.Logf("cleanup instance %s: %v", id, err)
				}
			})

			var imageID *string
			if err := database.DB.QueryRow(t.Context(),
				"SELECT image_id::text FROM instances WHERE id = $1", id).Scan(&imageID); err != nil {
				t.Fatalf("read back instance %s: %v", id, err)
			}
			got := ""
			if imageID != nil {
				got = *imageID
			}
			if got != tt.wantImageID {
				t.Errorf("instances.image_id = %q, want %q", got, tt.wantImageID)
			}
		})
	}
}
