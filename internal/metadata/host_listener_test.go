package metadata

import (
	"net/http"
	"testing"
)

// SPEC-033 T182: guests reach metadata through their network's namespace,
// which names them by network and address. The host's own listener named a caller by its address alone,
// in every network, so a host or pod on the node's network holding some guest's fixed IP read that
// guest's metadata and user_data. It names an instance only by X-Instance-ID now, from loopback or on a
// test stack that trusts the header (SPEC-033 FR-064).
func TestHostListenerNamesNoGuestByAddress_DB(t *testing.T) {
	requireDB(t)
	n := seedMDNetwork(t)
	guest := seedGuestOn(t, n, "198.51.100.70")
	r := newMetadataTestRouter()

	for _, path := range []string{"/latest/meta-data/instance-id", "/openstack/latest/user_data", "/openstack/latest/meta_data.json"} {
		if code, body := hostGet(r, path, "198.51.100.70", nil); code != http.StatusNotFound {
			t.Errorf("host listener, %s from the guest's address: got %d %q; want 404", path, code, body)
		}
	}
	if code, body := networkGet(r, n, "/latest/meta-data/instance-id", "198.51.100.70", nil); code != http.StatusOK || body != ec2InstanceID(guest) {
		t.Errorf("the guest through its network's namespace: got %d %q; want 200 %q", code, body, ec2InstanceID(guest))
	}
	if code, body := hostGet(r, "/latest/meta-data/instance-id", "127.0.0.1", map[string]string{"X-Instance-ID": guest}); code != http.StatusOK || body != ec2InstanceID(guest) {
		t.Errorf("host listener, X-Instance-ID from loopback: got %d %q; want 200 %q", code, body, ec2InstanceID(guest))
	}
}
