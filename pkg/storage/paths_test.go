package storage

import "testing"

// The node-local layout under /var/lib/o3k is a contract: the chart's persistent
// hostPath, Nova's overlay backing, and the volume attach/delete paths all depend
// on these exact paths (SPEC-009 WS-0/WS-5).
func TestLocalPaths(t *testing.T) {
	cases := []struct {
		name, got, want string
	}{
		{"volume dir", LocalVolumeDir(), "/var/lib/o3k/volumes"},
		{"volume path", LocalVolumePath("abc"), "/var/lib/o3k/volumes/volume-abc.qcow2"},
		{"image dir", LocalImageDir(), "/var/lib/o3k/images"},
		{"image path (raw base)", LocalImagePath("abc"), "/var/lib/o3k/images/image-abc.raw"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// RemoveLocalVolume tolerates an already-absent backing file, so the Nova
// delete-on-termination path never fails for a volume with no local backing
// (stub/RBD) — SPEC-009 FR-023.
func TestRemoveLocalVolumeMissingIsNil(t *testing.T) {
	if err := RemoveLocalVolume("nonexistent-volume-7f3a9c21"); err != nil {
		t.Errorf("RemoveLocalVolume(missing) = %v, want nil", err)
	}
}
