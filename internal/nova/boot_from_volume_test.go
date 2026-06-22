package nova

import (
	"encoding/json"
	"strings"
	"testing"
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
