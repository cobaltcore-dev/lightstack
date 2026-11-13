package nova

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestFlavorBoundsMatchFlamingo(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	// Flamingo parameter_types.flavor_param_{positive,non_negative} use
	// signed 32-bit limits. A zero root disk is valid for boot-from-volume.
	for _, tc := range []struct {
		name, fields string
		want         int
	}{
		{"zero-disk", `"ram":512,"vcpus":1,"disk":0`, http.StatusOK},
		{"upper-bound", `"ram":2147483647,"vcpus":2147483647,"disk":2147483647`, http.StatusOK},
		{"missing-disk", `"ram":512,"vcpus":1`, http.StatusBadRequest},
		{"zero-ram", `"ram":0,"vcpus":1,"disk":1`, http.StatusBadRequest},
		{"zero-cpu", `"ram":512,"vcpus":0,"disk":1`, http.StatusBadRequest},
		{"negative-disk", `"ram":512,"vcpus":1,"disk":-1`, http.StatusBadRequest},
		{"overflow-ram", `"ram":2147483648,"vcpus":1,"disk":1`, http.StatusBadRequest},
		{"overflow-cpu", `"ram":512,"vcpus":2147483648,"disk":1`, http.StatusBadRequest},
		{"overflow-disk", `"ram":512,"vcpus":1,"disk":2147483648`, http.StatusBadRequest},
		{"overflow-ephemeral", `"ram":512,"vcpus":1,"disk":1,"OS-FLV-EXT-DATA:ephemeral":2147483648`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "bounds-" + uuid.NewString()
			t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM flavors WHERE name=$1", name) })
			code, body := doReq(r, http.MethodPost, "/v2.1/flavors", fmt.Sprintf(`{"flavor":{"name":%q,%s}}`, name, tc.fields), operatorHdr("admin"))
			if code != tc.want {
				t.Errorf("create flavor: %d %s; want %d", code, body, tc.want)
			}
		})
	}
}
