package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// SPEC-011 T002 (cloud-storage#510): the local image store must resolve to the
// persistent node hostPath (/var/lib/lightstack/images), never ~/.lightstack/images which lives
// off the chart's mount. Glance was the lone service that silently fell back to
// ~/.lightstack when the canonical dir was absent at startup, so Nova — which always reads
// /var/lib/lightstack/images — could not find the base image and the VM booted to ERROR.
// Mirrors ceph_test.go for volumes.
func TestLocalImageDirIsPersistentHostPath(t *testing.T) {
	if got := LocalImageDir(); got != "/var/lib/lightstack/images" {
		t.Errorf("LocalImageDir() = %q, want /var/lib/lightstack/images", got)
	}
}

// NewImageStore must resolve localPath to the canonical node-local image dir in
// every mode, and never to a home-relative ~/.lightstack fallback — regardless of whether
// the canonical dir already exists on the test host (FR-002/003/006, SC-007).
func TestNewImageStoreUsesPersistentLocalPath(t *testing.T) {
	for _, mode := range []string{"local", "local,rbd", "stub"} {
		s, err := NewImageStore(mode, "images", "", S3Options{})
		if err != nil {
			t.Fatalf("NewImageStore(%q) error: %v", mode, err)
		}
		if s.localPath != "/var/lib/lightstack/images" {
			t.Errorf("mode %q: localPath = %q, want /var/lib/lightstack/images", mode, s.localPath)
		}
		if strings.Contains(s.localPath, "/.lightstack") || !strings.HasPrefix(s.localPath, "/var/lib/lightstack") {
			t.Errorf("mode %q: localPath %q must be the canonical node-local dir, never a ~/.lightstack fallback", mode, s.localPath)
		}
	}
}

// PR #73 review (r3710747125): the `rbd,s3` hybrid can never write its S3 replica.
// Glance's uploadImageRBD is a placeholder that always errors (there is no
// ceph-tagged variant), and the mode's upload path returned success without ever
// calling S3 — while DownloadImage's `rbd,s3` fallback reads an S3 object that was
// therefore never written. Reject the mode at startup (fail-fast) rather than accept
// a config that promises redundancy it cannot deliver.
func TestNewImageStoreRejectsRBDS3Hybrid(t *testing.T) {
	_, err := NewImageStore("rbd,s3", "images", "", S3Options{})
	if err == nil {
		t.Fatal(`NewImageStore("rbd,s3") must fail fast: Glance has no RBD backend, so the S3 replica is never written`)
	}
	if !strings.Contains(err.Error(), "local,s3") {
		t.Errorf("error must point at a supported mode, got: %v", err)
	}

	// Every supported mode must still construct.
	for _, mode := range []string{"stub", "local", "local,rbd", "local,s3"} {
		if _, err := NewImageStore(mode, "images", "", S3Options{}); err != nil {
			t.Errorf("NewImageStore(%q) must still succeed, got: %v", mode, err)
		}
	}
}

// The `rbd,s3` upload branch must never report success for a write that only went
// to RBD (the S3 replica is skipped because the reader is already consumed). The
// mode is rejected in NewImageStore, so this is belt-and-braces for anyone who
// implements the RBD backend later (PR #73 review r3710747125).
func TestUploadImageRBDS3NeverReportsPartialSuccess(t *testing.T) {
	s, err := NewImageStore("stub", "images", "", S3Options{})
	if err != nil {
		t.Fatalf("NewImageStore: %v", err)
	}
	s.mode = "rbd,s3" // bypass the startup guard to exercise the branch directly

	size, normalized, err := s.UploadImage(context.Background(), "img-1", strings.NewReader("data"))
	if err == nil {
		t.Fatalf("rbd,s3 upload reported success (size=%d normalized=%v); a half-written image must be an error", size, normalized)
	}
}

// EnsureLocalBase (SPEC-014 T014, FR-013/FR-017) must materialize the raw base
// by fetching from the backend, verify it against the expected checksum, and
// leave nothing behind on a mismatch. Uses stub mode + an overridden localPath
// so the test never touches /var/lib/lightstack.
func TestEnsureLocalBase_FetchVerifyChecksum(t *testing.T) {
	s, err := NewImageStore("stub", "images", "", S3Options{})
	if err != nil {
		t.Fatalf("NewImageStore: %v", err)
	}
	s.localPath = t.TempDir()

	const id = "img-1"
	payload := make([]byte, 100) // stub download returns zeros sized to the upload
	if _, _, err := s.UploadImage(context.Background(), id, bytes.NewReader(payload)); err != nil {
		t.Fatalf("UploadImage: %v", err)
	}
	want := fmt.Sprintf("%x", md5.Sum(payload))
	base := filepath.Join(s.localPath, imageFileName(id))

	// No expected checksum → fetch + normalize succeeds.
	got, err := s.EnsureLocalBase(context.Background(), id, "")
	if err != nil {
		t.Fatalf("EnsureLocalBase(no checksum): %v", err)
	}
	if got != base {
		t.Fatalf("EnsureLocalBase path = %q, want %q", got, base)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("base not materialized: %v", err)
	}

	// Matching checksum → succeeds.
	_ = os.Remove(base)
	if _, err := s.EnsureLocalBase(context.Background(), id, want); err != nil {
		t.Fatalf("EnsureLocalBase(matching checksum): %v", err)
	}

	// Mismatching checksum → error, and no base is left behind (never boot a
	// corrupt image).
	_ = os.Remove(base)
	if _, err := s.EnsureLocalBase(context.Background(), id, "ffffffffffffffffffffffffffffffff"); err == nil {
		t.Fatal("expected checksum-mismatch error, got nil")
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("base must not exist after checksum mismatch; stat err = %v", err)
	}
}

// EnsureLocalBase must return an already-present base without touching the
// backend (fast path).
func TestEnsureLocalBase_FastPathWhenPresent(t *testing.T) {
	s, err := NewImageStore("stub", "images", "", S3Options{})
	if err != nil {
		t.Fatalf("NewImageStore: %v", err)
	}
	s.localPath = t.TempDir()

	const id = "img-2" // deliberately NOT uploaded to the stub backend
	base := filepath.Join(s.localPath, imageFileName(id))
	if err := os.WriteFile(base, []byte("raw-bytes"), 0o644); err != nil {
		t.Fatalf("seed base: %v", err)
	}
	got, err := s.EnsureLocalBase(context.Background(), id, "")
	if err != nil {
		t.Fatalf("EnsureLocalBase fast path: %v", err)
	}
	if got != base {
		t.Fatalf("EnsureLocalBase path = %q, want %q", got, base)
	}
}

// s3PartSizeFor (SPEC-014 FR-014) must pick a part size that keeps a multipart
// upload within S3's 10,000-part limit while never dropping below the 5 MiB
// minimum — otherwise a ~10 GB (or larger) image would fail to upload.
func TestS3PartSizeFor(t *testing.T) {
	for _, size := range []int64{
		1,
		10 * 1024 * 1024,
		10 * 1024 * 1024 * 1024,       // ~10 GB target
		4 * 1024 * 1024 * 1024 * 1024, // 4 TiB — forces parts above the default
	} {
		part := s3PartSizeFor(size)
		if part < s3MinPartSize {
			t.Errorf("size %d: part %d below S3 minimum %d", size, part, s3MinPartSize)
		}
		if parts := (size + part - 1) / part; parts > s3MaxParts {
			t.Errorf("size %d: part %d needs %d parts, exceeds S3 max %d", size, part, parts, s3MaxParts)
		}
	}
}

// UploadImage must report whether it normalized the stored bytes to raw so the
// caller records disk_format honestly (SPEC-014): local normalizes, stub does not.
func TestUploadImage_NormalizedToRawFlag(t *testing.T) {
	// stub backend stores bytes as-is → not normalized.
	s, err := NewImageStore("stub", "images", "", S3Options{})
	if err != nil {
		t.Fatalf("NewImageStore(stub): %v", err)
	}
	s.localPath = t.TempDir()
	if _, normalized, err := s.UploadImage(context.Background(), "img-stub", bytes.NewReader([]byte("data"))); err != nil {
		t.Fatalf("UploadImage(stub): %v", err)
	} else if normalized {
		t.Error("stub upload must report normalizedToRaw=false")
	}

	// local backend normalizes to raw → normalized=true, and a qcow2 lands as raw.
	requireQemuImg(t)
	ls, err := NewImageStore("local", "images", "", S3Options{})
	if err != nil {
		t.Fatalf("NewImageStore(local): %v", err)
	}
	ls.localPath = t.TempDir()
	f, err := os.Open(makeQcow2(t, t.TempDir()))
	if err != nil {
		t.Fatalf("open qcow2: %v", err)
	}
	defer f.Close()
	_, normalized, err := ls.UploadImage(context.Background(), "img-local", f)
	if err != nil {
		t.Fatalf("UploadImage(local): %v", err)
	}
	if !normalized {
		t.Error("local upload must report normalizedToRaw=true")
	}
	if format, err := detectDiskFormat(filepath.Join(ls.localPath, imageFileName("img-local"))); err != nil || format != "raw" {
		t.Errorf("stored disk format = %q (err %v), want raw", format, err)
	}
}

// normalizeToRaw must convert atomically: a qcow2 becomes a raw dst with no temp
// left behind, and a FAILED convert must leave no file at dst — a boot trusts an
// existing base without re-verifying it, so a partial base would boot corrupt
// (SPEC-014 review, crash-safety).
func TestNormalizeToRaw_AtomicConvert(t *testing.T) {
	requireQemuImg(t)

	// Success: qcow2 src → raw dst; src consumed; no .convert temp left behind.
	dir := t.TempDir()
	src := makeQcow2(t, dir)
	dst := filepath.Join(dir, "base.raw")
	if err := normalizeToRaw(src, dst); err != nil {
		t.Fatalf("normalizeToRaw: %v", err)
	}
	if format, err := detectDiskFormat(dst); err != nil || format != "raw" {
		t.Fatalf("dst format = %q (err %v), want raw", format, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source must be consumed; stat err = %v", err)
	}
	if _, err := os.Stat(dst + ".convert"); !os.IsNotExist(err) {
		t.Errorf(".convert temp must not be left behind; stat err = %v", err)
	}

	// Failure: force the convert to fail by occupying the temp path with a
	// directory qemu-img cannot write over, then assert dst was never created.
	dir2 := t.TempDir()
	src2 := makeQcow2(t, dir2)
	dst2 := filepath.Join(dir2, "base.raw")
	if err := os.Mkdir(dst2+".convert", 0o755); err != nil {
		t.Fatalf("seed blocking dir: %v", err)
	}
	if err := normalizeToRaw(src2, dst2); err == nil {
		t.Fatal("expected error when the convert target is unwritable, got nil")
	}
	if _, err := os.Stat(dst2); !os.IsNotExist(err) {
		t.Errorf("dst must not exist after a failed convert; stat err = %v", err)
	}
}

func requireQemuImg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
}

// makeQcow2 writes a 1 MiB raw file and converts it to qcow2, returning the qcow2
// path (the intermediate raw is removed).
func makeQcow2(t *testing.T, dir string) string {
	t.Helper()
	raw := filepath.Join(dir, "src.raw")
	if err := os.WriteFile(raw, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	qcow := filepath.Join(dir, "src.qcow2")
	if out, err := exec.Command("qemu-img", "convert", "-O", "qcow2", raw, qcow).CombinedOutput(); err != nil {
		t.Fatalf("make qcow2: %v: %s", err, out)
	}
	_ = os.Remove(raw)
	return qcow
}

// TestStoredChecksumMatchesStoredBytesNotUploadBytes pins the stored-object
// checksum behavior: normalizing backends rewrite the upload (qcow2 → raw base), so the
// checksum recorded for an image must describe the STORED bytes — downloads
// serve them and EnsureLocalBase verifies a lazily fetched base against the
// recorded value on every boot. Hashing the upload stream instead made
// MD5(raw replica) != MD5(original qcow2), so boot-from-image failed on any
// node without the local base.
func TestStoredChecksumMatchesStoredBytesNotUploadBytes(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available")
	}
	s, err := NewImageStore("local", "", "", S3Options{})
	if err != nil {
		t.Fatalf("NewImageStore: %v", err)
	}
	s.localPath = t.TempDir() // same-package override: keep the test off /var/lib

	// A real qcow2: not raw bytes, so normalizeToRaw must convert it.
	qcow2Path := filepath.Join(t.TempDir(), "disk.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", qcow2Path, "1M").CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create: %v: %s", err, out)
	}
	qcow2Bytes, err := os.ReadFile(qcow2Path)
	if err != nil {
		t.Fatal(err)
	}

	imageID := "11111111-2222-3333-4444-555555555555"
	size, normalized, err := s.UploadImage(context.Background(), imageID, bytes.NewReader(qcow2Bytes))
	if err != nil {
		t.Fatalf("UploadImage: %v", err)
	}
	if !normalized {
		t.Fatal("local mode must report normalization")
	}
	// The stored size is the raw virtual size, not the qcow2 file size.
	if size != 1<<20 {
		t.Errorf("stored size = %d; want the raw size %d", size, 1<<20)
	}

	streamHash := fmt.Sprintf("%x", md5.Sum(qcow2Bytes))
	stored, err := s.StoredChecksum(context.Background(), imageID)
	if err != nil {
		t.Fatalf("StoredChecksum: %v", err)
	}
	storedBytes, err := os.ReadFile(filepath.Join(s.localPath, imageFileName(imageID)))
	if err != nil {
		t.Fatalf("read stored image: %v", err)
	}
	wantHash := fmt.Sprintf("%x", md5.Sum(storedBytes))

	if stored != wantHash {
		t.Errorf("StoredChecksum = %s; want MD5 of the stored bytes %s", stored, wantHash)
	}
	if stored == streamHash {
		t.Error("StoredChecksum equals the upload-stream hash; the qcow2 bytes were normalized, so the hashes must differ — this is the N3 regression")
	}
}
