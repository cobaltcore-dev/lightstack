package storage

import (
	"strings"
	"testing"
)

// SPEC-011 T002 (cloud-storage#510): the local image store must resolve to the
// persistent node hostPath (/var/lib/o3k/images), never ~/.o3k/images which lives
// off the chart's mount. Glance was the lone service that silently fell back to
// ~/.o3k when the canonical dir was absent at startup, so Nova — which always reads
// /var/lib/o3k/images — could not find the base image and the VM booted to ERROR.
// Mirrors ceph_test.go for volumes.
func TestLocalImageDirIsPersistentHostPath(t *testing.T) {
	if got := LocalImageDir(); got != "/var/lib/o3k/images" {
		t.Errorf("LocalImageDir() = %q, want /var/lib/o3k/images", got)
	}
}

// NewImageStore must resolve localPath to the canonical node-local image dir in
// every mode, and never to a home-relative ~/.o3k fallback — regardless of whether
// the canonical dir already exists on the test host (FR-002/003/006, SC-007).
func TestNewImageStoreUsesPersistentLocalPath(t *testing.T) {
	for _, mode := range []string{"local", "local,rbd", "stub"} {
		s, err := NewImageStore(mode, "images", "", "", "", "")
		if err != nil {
			t.Fatalf("NewImageStore(%q) error: %v", mode, err)
		}
		if s.localPath != "/var/lib/o3k/images" {
			t.Errorf("mode %q: localPath = %q, want /var/lib/o3k/images", mode, s.localPath)
		}
		if strings.Contains(s.localPath, "/.o3k") || !strings.HasPrefix(s.localPath, "/var/lib/o3k") {
			t.Errorf("mode %q: localPath %q must be the canonical node-local dir, never a ~/.o3k fallback", mode, s.localPath)
		}
	}
}
