package metadata

import (
	"net/http"
	"testing"
)

// The EC2 tree's instance-id must look like an EC2 one. cirros's EC2 datasource accepts
// an id only once it starts with "i-", retrying 20 times, 2s apart, before settling for
// what it got, so the bare UUID cost every cirros boot 40 seconds. 17 hex digits is AWS's
// current id length; upstream Nova serves i-%08x from a counter table.
func TestEC2InstanceIDIsEC2Shaped(t *testing.T) {
	r := newMetadataTestRouter()
	const id = "755a18e9-eb39-40f6-8208-267410268522"
	for _, version := range []string{"/2009-04-04", "/latest"} {
		code, body := metaGet(r, version+"/meta-data/instance-id", id)
		if code != http.StatusOK || body != "i-755a18e9eb3940f68" {
			t.Errorf("GET %s/meta-data/instance-id = %d %q; want 200 %q",
				version, code, body, "i-755a18e9eb3940f68")
		}
	}
}
