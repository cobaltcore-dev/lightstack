package storage

import "testing"

// SPEC-009 T005: local volumes must live on the persistent node hostPath
// (/var/lib/o3k/volumes), not ~/.o3k/volumes which is off the chart's mount and
// did not survive pod restarts (FR-006a).
func TestLocalVolumeDirIsPersistentHostPath(t *testing.T) {
	if got := LocalVolumeDir(); got != "/var/lib/o3k/volumes" {
		t.Errorf("LocalVolumeDir() = %q, want /var/lib/o3k/volumes", got)
	}
}

func TestNewCephClientUsesPersistentLocalPath(t *testing.T) {
	c := NewCephClient("stub", "volumes", "")
	if c.localPath != "/var/lib/o3k/volumes" {
		t.Errorf("CephClient.localPath = %q, want /var/lib/o3k/volumes", c.localPath)
	}
}
