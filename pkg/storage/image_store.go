package storage

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/cobaltcore-dev/lightstack/pkg/paths"
	"github.com/rs/zerolog/log"
)

// ImageStore manages image storage operations
type ImageStore struct {
	mode                 string // "stub", "local", "rbd", "s3", or combinations like "local,rbd", "local,s3"
	cephPool             string
	cephConf             string
	s3Bucket             string
	s3Region             string
	s3Endpoint           string
	s3CABundle           string        // PEM CA bundle for an internal-CA HTTPS RGW (optional)
	s3InsecureSkipVerify bool          // test-only: skip TLS verification
	connectTimeout       time.Duration // connect + time-to-first-byte fail-fast (NOT total transfer)
	timeout              time.Duration // short deadline for metadata ops (head/delete)
	localPath            string
	mu                   sync.Mutex
	fetchLocks           sync.Map              // imageID -> *sync.Mutex; serializes EnsureLocalBase
	stubImages           map[string]*stubImage // For stub mode
	s3Client             *s3.Client
}

// S3Options carries the S3/RGW client settings. It lets the settings grow
// (CA bundle, TLS, timeouts) without churning the NewImageStore/NewService
// positional signatures (plan.md research §12).
type S3Options struct {
	Bucket             string
	Region             string
	Endpoint           string
	CABundlePath       string
	InsecureSkipVerify bool
	// ConnectTimeout bounds connect + time-to-first-byte only (fail-fast). It
	// does NOT bound the body transfer, so large images stream to completion
	// (FR-014). Zero applies a default.
	ConnectTimeout time.Duration
}

// stubImage represents a simulated image
type stubImage struct {
	id        string
	size      int64
	createdAt time.Time
}

// --- Image artifact naming: single source of truth (SPEC-009 FR-003) ---
// Shared by Glance's write sites here and Nova's read site (the per-instance
// overlay's backing file), so the write and boot locations never diverge.

// LocalImageDir is the node-local directory for raw image bases in real mode
// (the chart's persistent hostPath).
func LocalImageDir() string { return paths.ImagesDir() }

// imageFileName is the on-disk filename for a stored image (raw base).
func imageFileName(imageID string) string { return "image-" + imageID + ".raw" }

// LocalImagePath is the canonical raw-base path for an image in real mode;
// Nova's per-instance overlay backs off this file.
func LocalImagePath(imageID string) string {
	return filepath.Join(LocalImageDir(), imageFileName(imageID))
}

// rbdImageName is the RBD object name for a stored image.
func rbdImageName(imageID string) string { return "image-" + imageID }

// s3ImageKey is the S3 object key for a stored image.
func s3ImageKey(imageID string) string { return "images/" + imageFileName(imageID) }

// NewImageStore creates a new image store.
//
// Failure policy follows the project's "Fail-Fast Design" principle
// (CLAUDE.md / docs/ARCHITECTURE.md) for external dependencies, combined
// with the per-mode behaviour documented in docs/STORAGE_MODES.md:
//
//   - mode == "s3" (S3 is the only backend): an S3 client init failure is
//     fatal — return the error so the caller (Glance) can refuse to start.
//   - hybrid modes containing "s3" (e.g. "local,s3", "rbd,s3"): log the
//     error and continue. The documented "Automatic S3 fallback" lets the
//     primary backend serve traffic; S3 operations will surface
//     "S3 client not initialized" until the config is corrected.
func NewImageStore(mode, cephPool, cephConf string, s3opts S3Options) (*ImageStore, error) {
	// Reject `rbd,s3` up front: Glance has no RBD backend (uploadImageRBD /
	// downloadImageRBD are placeholders in every build — unlike Cinder there is no
	// ceph-tagged variant), so the mode can never write the S3 replica that
	// DownloadImage's rbd,s3 fallback then tries to read. Fail fast at startup
	// instead of accepting a config that promises redundancy it cannot deliver
	// (PR #73 review r3710747125).
	if mode == "rbd,s3" {
		return nil, fmt.Errorf("glance storage_mode %q is not supported: the Glance RBD backend is not implemented, "+
			"so the S3 replica would never be written; use \"local,s3\" (local primary, S3 replica) or \"s3\"", mode)
	}

	// Always resolve to the canonical node-local image dir (the chart's
	// persistent hostPath). SPEC-011 / cloud-storage#510: Glance previously fell
	// back to ~/.lightstack/images when this dir was absent at startup, but Nova always
	// reads LocalImagePath() under /var/lib/lightstack/images — so the fallback silently
	// diverged the write and read locations and VMs booted to ERROR. Mirror the
	// other local-storage services (see ceph.go): resolve unconditionally and
	// create it (below); never relocate.
	localPath := LocalImageDir()

	store := &ImageStore{
		mode:                 mode,
		cephPool:             cephPool,
		cephConf:             cephConf,
		s3Bucket:             s3opts.Bucket,
		s3Region:             s3opts.Region,
		s3Endpoint:           s3opts.Endpoint,
		s3CABundle:           s3opts.CABundlePath,
		s3InsecureSkipVerify: s3opts.InsecureSkipVerify,
		connectTimeout:       s3opts.ConnectTimeout,
		timeout:              5 * time.Second,
		localPath:            localPath,
		stubImages:           make(map[string]*stubImage),
	}

	// Create local storage directory if needed. Surface a failure (FR-008) rather
	// than masking it by writing image data to an unread location.
	if mode == "local" || containsMode(mode, "local") {
		if err := os.MkdirAll(store.localPath, 0755); err != nil {
			log.Error().Err(err).Str("path", store.localPath).
				Msg("failed to create local image directory")
		}
	}

	// Initialize S3 client if needed
	if mode == "s3" || containsMode(mode, "s3") {
		if err := store.initS3Client(); err != nil {
			if mode == "s3" {
				return nil, fmt.Errorf("S3 is the configured storage mode but S3 client init failed: %w", err)
			}
			log.Error().Err(err).
				Str("mode", mode).
				Str("region", s3opts.Region).
				Str("endpoint", s3opts.Endpoint).
				Msg("S3 client init failed; falling back to other configured backend(s)")
		}
	}

	return store, nil
}

// containsMode checks if a mode string contains a specific mode
func containsMode(modes, target string) bool {
	for i := 0; i < len(modes); {
		// Find next comma or end
		end := i
		for end < len(modes) && modes[end] != ',' {
			end++
		}
		if modes[i:end] == target {
			return true
		}
		i = end + 1
	}
	return false
}

// initS3Client initializes the S3 client
func (s *ImageStore) initS3Client() error {
	ctx := context.Background()

	httpClient, err := s.newS3HTTPClient()
	if err != nil {
		return err
	}

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(s.s3Region),
		config.WithHTTPClient(httpClient),
	)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Use custom endpoint if provided (for S3-compatible storage like MinIO, Ceph RGW)
	if s.s3Endpoint != "" {
		s.s3Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(s.s3Endpoint)
			o.UsePathStyle = true // Required for MinIO and some S3-compatible stores
		})
	} else {
		s.s3Client = s3.NewFromConfig(cfg)
	}

	return nil
}

// newS3HTTPClient builds the HTTP client used for S3/RGW. TLS optionally trusts
// a custom/internal CA bundle (FR-015); the fail-fast budget is scoped to
// connect + time-to-first-byte (dial / TLS handshake / response headers) so
// large image bodies stream to completion (FR-014) instead of being cut off by
// a fixed total-operation timeout.
func (s *ImageStore) newS3HTTPClient() (*awshttp.BuildableClient, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.s3InsecureSkipVerify {
		tlsCfg.InsecureSkipVerify = true // #nosec G402 -- opt-in, test-only (FR-015)
	}
	if s.s3CABundle != "" {
		pemBytes, err := os.ReadFile(s.s3CABundle)
		if err != nil {
			return nil, fmt.Errorf("failed to read S3 CA bundle %q: %w", s.s3CABundle, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("no certificates found in S3 CA bundle %q", s.s3CABundle)
		}
		tlsCfg.RootCAs = pool
	}

	firstByte := s.connectTimeout
	if firstByte <= 0 {
		firstByte = 10 * time.Second
	}

	return awshttp.NewBuildableClient().
		WithDialerOptions(func(d *net.Dialer) {
			d.Timeout = firstByte // connect fail-fast
		}).
		WithTransportOptions(func(tr *http.Transport) {
			tr.TLSClientConfig = tlsCfg
			tr.TLSHandshakeTimeout = firstByte
			tr.ResponseHeaderTimeout = firstByte // time-to-first-byte; does NOT bound the body
			tr.ExpectContinueTimeout = 1 * time.Second
		}), nil
}

// UploadImage uploads an image to storage. The bool return reports whether the
// backend normalized the stored bytes to raw on ingest: the local/RBD real-mode
// backends do (SPEC-009 WS-0/FR-003), so the caller should record disk_format=raw;
// the S3 backend stores the bytes as-is, so the caller must keep the image's
// declared disk_format (SPEC-014 — S3 is out of the normalize-on-ingest scope).
func (s *ImageStore) UploadImage(ctx context.Context, imageID string, reader io.Reader) (int64, bool, error) {
	switch s.mode {
	case "stub":
		size, err := s.uploadImageStub(imageID, reader)
		return size, false, err
	case "local":
		size, err := s.uploadImageLocal(ctx, imageID, reader)
		return size, true, err
	case "rbd":
		size, err := s.uploadImageRBD(ctx, imageID, reader)
		return size, true, err
	case "s3":
		size, err := s.uploadImageS3(ctx, imageID, reader)
		return size, false, err
	case "local,rbd":
		// Store locally (normalized to raw); RBD replication is not implemented yet.
		size, err := s.uploadImageLocal(ctx, imageID, reader)
		if err != nil {
			return 0, false, fmt.Errorf("failed to upload to local: %w", err)
		}
		return size, true, nil
	case "local,s3":
		// Upload to local first (normalizes to raw); the S3 replica is that raw file.
		size, err := s.uploadImageLocal(ctx, imageID, reader)
		if err != nil {
			return 0, false, fmt.Errorf("failed to upload to local: %w", err)
		}
		// Then upload to S3 from local file
		if err := s.uploadLocalToS3(ctx, imageID); err != nil {
			// Keep local copy even if S3 fails
			return size, true, fmt.Errorf("uploaded to local but failed to replicate to S3: %w", err)
		}
		return size, true, nil
	case "rbd,s3":
		// Unreachable in practice — NewImageStore rejects this mode. Kept fail-closed
		// so that whoever implements the RBD backend cannot accidentally ship a
		// half-written image reported as success: the reader is consumed by the RBD
		// upload, so the S3 replica is never written and DownloadImage's rbd,s3
		// fallback would find nothing (PR #73 review r3710747125).
		if _, err := s.uploadImageRBD(ctx, imageID, reader); err != nil {
			return 0, false, fmt.Errorf("failed to upload to RBD: %w", err)
		}
		return 0, false, fmt.Errorf("storage mode %q cannot replicate to S3: the image reader is consumed by the RBD "+
			"upload, so only RBD was written; use \"local,s3\" instead", s.mode)
	default:
		return 0, false, fmt.Errorf("unsupported storage mode: %s", s.mode)
	}
}

// ctxReader wraps an io.Reader so that long-running copies honor a
// context's deadline or cancellation. Local file I/O doesn't natively
// respect context.Context, so we check between Read calls.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

// uploadImageStub simulates image upload
func (s *ImageStore) uploadImageStub(imageID string, reader io.Reader) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Read all data to get size
	data, err := io.ReadAll(reader)
	if err != nil {
		return 0, err
	}

	s.stubImages[imageID] = &stubImage{
		id:        imageID,
		size:      int64(len(data)),
		createdAt: time.Now(),
	}

	return int64(len(data)), nil
}

// uploadImageLocal uploads an image to local storage, normalizing it to a raw
// base on ingest (SPEC-009 WS-0/FR-003): VMs boot from a qcow2 overlay backing
// this raw file. The upload streams to a temp file, then qemu-img converts it
// to raw (or renames it if already raw / qemu-img is unavailable).
func (s *ImageStore) uploadImageLocal(ctx context.Context, imageID string, reader io.Reader) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	imagePath := filepath.Join(s.localPath, imageFileName(imageID))
	tmpPath := imagePath + ".upload"

	// Stream the upload to a temp file. Wrap the source so the timeout fires —
	// os.File doesn't natively respect context.
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return 0, fmt.Errorf("failed to create image file: %w", err)
	}
	size, copyErr := io.Copy(tmp, &ctxReader{ctx: ctx, r: reader})
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("failed to write image data: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("failed to close image file: %w", closeErr)
	}

	// Normalize to a raw base.
	if err := normalizeToRaw(tmpPath, imagePath); err != nil {
		_ = os.Remove(tmpPath)
		_ = os.Remove(imagePath)
		return 0, fmt.Errorf("failed to normalize image to raw: %w", err)
	}

	// Report the stored (raw) size when available; fall back to the upload size.
	if fi, statErr := os.Stat(imagePath); statErr == nil {
		size = fi.Size()
	}
	return size, nil
}

// normalizeToRaw places a raw image at dst from src using qemu-img: if src is
// already raw it is renamed; non-raw (e.g. qcow2) is converted; if qemu-img is
// unavailable the bytes are kept as-is (best-effort — real-mode images expect
// qemu-img present, see build/package/Dockerfile).
func normalizeToRaw(src, dst string) error {
	format, err := detectDiskFormat(src)
	if err != nil {
		log.Warn().Err(err).Str("src", src).Msg("could not detect image format (qemu-img?); storing as-is")
		return os.Rename(src, dst)
	}
	if format == "raw" {
		return os.Rename(src, dst)
	}
	// Convert to a sibling temp file, then atomically rename into place. Writing dst
	// directly would leave a partial raw file there if the convert is interrupted —
	// and the boot fast path trusts an existing base without re-verifying it, so it
	// would boot a corrupt image.
	tmpDst := dst + ".convert"
	cmd := exec.Command("qemu-img", "convert", "-O", "raw", src, tmpDst)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmpDst)
		return fmt.Errorf("qemu-img convert %s->raw: %w: %s", format, err, string(out))
	}
	if err := os.Rename(tmpDst, dst); err != nil {
		_ = os.Remove(tmpDst)
		return fmt.Errorf("rename converted image into place: %w", err)
	}
	return os.Remove(src)
}

// detectDiskFormat returns qemu-img's detected disk format of path (e.g. "raw",
// "qcow2"). It errors when qemu-img is unavailable or the file is unreadable.
func detectDiskFormat(path string) (string, error) {
	out, err := exec.Command("qemu-img", "info", "--output=json", path).Output()
	if err != nil {
		return "", err
	}
	var info struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("parse qemu-img info: %w", err)
	}
	return info.Format, nil
}

// uploadImageRBD uploads image to RBD. This is a stub pending go-ceph
// integration; once wired up, restore the WithTimeout(ctx, s.timeout)
// guard around the exec.CommandContext call.
func (s *ImageStore) uploadImageRBD(_ context.Context, imageID string, _ io.Reader) (int64, error) {
	imageName := rbdImageName(imageID)

	// TODO: Use go-ceph to write to RBD
	// ctx, cancel := context.WithTimeout(ctx, s.timeout); defer cancel()
	// cmd := exec.CommandContext(ctx, "rbd", "import", "-", fmt.Sprintf("%s/%s", s.cephPool, imageName))
	// cmd.Stdin = reader
	// return cmd.Run()

	return 0, fmt.Errorf("ceph cluster not configured (would upload to %s/%s)", s.cephPool, imageName)
}

// uploadImageS3 uploads image to S3. No total-operation timeout is applied so
// large images stream to completion (FR-014); connect/first-byte fail-fast is
// enforced by the HTTP transport (newS3HTTPClient), and cancellation still
// flows through ctx.
func (s *ImageStore) uploadImageS3(ctx context.Context, imageID string, reader io.Reader) (int64, error) {
	if s.s3Client == nil {
		return 0, fmt.Errorf("S3 client not initialized")
	}

	objectKey := s3ImageKey(imageID)

	// Stream to a temp file so we can provide an io.ReadSeeker to the S3 SDK
	// without buffering the entire image in memory. Stage on the node-local images
	// volume (localPath), not $TMPDIR — the latter is often small or RAM-backed and
	// would ENOSPC/OOM on a multi-GB image (FR-014).
	if err := os.MkdirAll(s.localPath, 0o755); err != nil {
		return 0, fmt.Errorf("failed to create S3 staging dir %q: %w", s.localPath, err)
	}
	tmp, err := os.CreateTemp(s.localPath, "s3upload-*")
	if err != nil {
		return 0, fmt.Errorf("failed to create temp file for S3 upload: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	size, err := io.Copy(tmp, reader)
	if err != nil {
		return 0, fmt.Errorf("failed to buffer image for S3 upload: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("failed to seek temp file: %w", err)
	}

	// Objects over the single-PUT limit must go via multipart, or S3/RGW rejects
	// the PUT (EntityTooLarge) — which would break FR-014 (validated up to ~10 GB).
	if size > s3SinglePutMax {
		if err := s.uploadS3Multipart(ctx, objectKey, tmp, size); err != nil {
			return 0, err
		}
		return size, nil
	}

	if _, err := s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.s3Bucket),
		Key:           aws.String(objectKey),
		Body:          tmp,
		ContentLength: aws.Int64(size),
	}); err != nil {
		return 0, fmt.Errorf("failed to upload to S3: %w", err)
	}

	return size, nil
}

// S3 multipart-upload constraints (AWS S3, honored by Ceph RGW).
const (
	s3SinglePutMax = 5 * 1024 * 1024 * 1024 // 5 GiB — hard cap for a single PutObject
	s3MinPartSize  = 5 * 1024 * 1024        // 5 MiB — minimum part size (all but the last)
	s3MaxParts     = 10000                  // maximum parts per multipart upload
)

// s3DefaultPartSize is the default multipart chunk size.
const s3DefaultPartSize int64 = 128 * 1024 * 1024 // 128 MiB

// s3PartSizeFor returns the multipart part size for an object of the given size:
// the default chunk, grown as needed to keep the part count within S3's 10,000-part
// limit, and never below the 5 MiB minimum.
func s3PartSizeFor(size int64) int64 {
	part := s3DefaultPartSize
	if byCount := (size + s3MaxParts - 1) / s3MaxParts; byCount > part {
		part = byCount
	}
	if part < s3MinPartSize {
		part = s3MinPartSize
	}
	return part
}

// uploadS3Multipart uploads r (of known size) to objectKey as an S3 multipart
// upload, aborting on any error so no orphaned parts are left billed. Used for
// objects larger than the single-PUT limit (FR-014).
func (s *ImageStore) uploadS3Multipart(ctx context.Context, objectKey string, r io.ReaderAt, size int64) error {
	create, err := s.s3Client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.s3Bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return fmt.Errorf("failed to start S3 multipart upload: %w", err)
	}
	uploadID := create.UploadId

	// abort cleans up the upload so failed parts aren't left billed. It runs on a
	// non-cancelled context so cleanup still happens when the request ctx was the
	// thing that failed (the transport keeps its own connect/first-byte timeouts).
	abort := func() {
		_, _ = s.s3Client.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(s.s3Bucket),
			Key:      aws.String(objectKey),
			UploadId: uploadID,
		})
	}

	partSize := s3PartSizeFor(size)
	var parts []types.CompletedPart
	var partNum int32 = 1
	for offset := int64(0); offset < size; offset += partSize {
		n := partSize
		if offset+n > size {
			n = size - offset
		}
		out, err := s.s3Client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(s.s3Bucket),
			Key:           aws.String(objectKey),
			UploadId:      uploadID,
			PartNumber:    aws.Int32(partNum),
			Body:          io.NewSectionReader(r, offset, n),
			ContentLength: aws.Int64(n),
		})
		if err != nil {
			abort()
			return fmt.Errorf("failed to upload S3 part %d: %w", partNum, err)
		}
		parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(partNum)})
		partNum++
	}

	if _, err := s.s3Client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.s3Bucket),
		Key:             aws.String(objectKey),
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		abort()
		return fmt.Errorf("failed to complete S3 multipart upload: %w", err)
	}
	return nil
}

// uploadLocalToS3 uploads an image from local storage to S3
func (s *ImageStore) uploadLocalToS3(ctx context.Context, imageID string) error {
	imagePath := filepath.Join(s.localPath, imageFileName(imageID))

	file, err := os.Open(imagePath)
	if err != nil {
		return fmt.Errorf("failed to open local image: %w", err)
	}
	defer file.Close()

	objectKey := s3ImageKey(imageID)

	_, err = s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.s3Bucket),
		Key:    aws.String(objectKey),
		Body:   file,
	})

	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	return nil
}

// StoredChecksum returns the MD5 hex digest of the image's stored local bytes.
// Normalizing backends (local, local,rbd, local,s3) rewrite the upload — a
// qcow2 becomes a raw base — so the checksum Glance advertises, downloads
// serve and EnsureLocalBase verifies against must be taken from the stored
// object, not from the client's upload stream (review 2026-10-13 N3: the
// stream hash made boot-from-image fail on any node without the local base,
// because MD5(raw replica) never equals MD5(original qcow2)). For S3-only and
// stub modes the stored bytes are the upload bytes, so the stream hash the
// caller already computed is the stored checksum and this method is not
// consulted. A future RBD-only backend should hash its own object instead.
func (s *ImageStore) StoredChecksum(ctx context.Context, imageID string) (string, error) {
	imagePath := filepath.Join(s.localPath, imageFileName(imageID))
	f, err := os.Open(imagePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New() // #nosec G401 -- Glance checksum is MD5 (compatibility), not a security control
	if _, err := io.Copy(h, &ctxReader{ctx: ctx, r: f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DownloadImage downloads an image from storage
func (s *ImageStore) DownloadImage(ctx context.Context, imageID string, writer io.Writer) error {
	switch s.mode {
	case "stub":
		return s.downloadImageStub(imageID, writer)
	case "local":
		return s.downloadImageLocal(ctx, imageID, writer)
	case "rbd":
		return s.downloadImageRBD(ctx, imageID, writer)
	case "s3":
		return s.downloadImageS3(ctx, imageID, writer)
	case "local,rbd":
		// Try local first (faster)
		if err := s.downloadImageLocal(ctx, imageID, writer); err == nil {
			return nil
		}
		// Fallback to RBD
		return s.downloadImageRBD(ctx, imageID, writer)
	case "local,s3":
		// Try local first (faster)
		if err := s.downloadImageLocal(ctx, imageID, writer); err == nil {
			return nil
		}
		// Fallback to S3
		return s.downloadImageS3(ctx, imageID, writer)
	case "rbd,s3":
		// Try RBD first
		if err := s.downloadImageRBD(ctx, imageID, writer); err == nil {
			return nil
		}
		// Fallback to S3
		return s.downloadImageS3(ctx, imageID, writer)
	default:
		return fmt.Errorf("unsupported storage mode: %s", s.mode)
	}
}

// downloadImageStub simulates image download
func (s *ImageStore) downloadImageStub(imageID string, writer io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	img, exists := s.stubImages[imageID]
	if !exists {
		return fmt.Errorf("image %s not found", imageID)
	}

	// Write fake data
	_, err := writer.Write(make([]byte, img.size))
	return err
}

// downloadImageLocal downloads from local storage
func (s *ImageStore) downloadImageLocal(ctx context.Context, imageID string, writer io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	imagePath := filepath.Join(s.localPath, imageFileName(imageID))

	file, err := os.Open(imagePath)
	if err != nil {
		return fmt.Errorf("failed to open image file: %w", err)
	}
	defer file.Close()

	// Wrap the source so the timeout actually fires — os.File doesn't
	// natively respect context.
	_, err = io.Copy(writer, &ctxReader{ctx: ctx, r: file})
	return err
}

// downloadImageRBD downloads from RBD. This is a stub pending go-ceph
// integration; once wired up, restore the WithTimeout(ctx, s.timeout)
// guard around the exec.CommandContext call.
func (s *ImageStore) downloadImageRBD(_ context.Context, imageID string, _ io.Writer) error {
	imageName := rbdImageName(imageID)

	// TODO: Use go-ceph
	// ctx, cancel := context.WithTimeout(ctx, s.timeout); defer cancel()
	// cmd := exec.CommandContext(ctx, "rbd", "export", fmt.Sprintf("%s/%s", s.cephPool, imageName), "-")
	// cmd.Stdout = writer
	// return cmd.Run()

	return fmt.Errorf("ceph cluster not configured (would download from %s/%s)", s.cephPool, imageName)
}

// downloadImageS3 downloads from S3. The body streams under the caller's
// context with no total-operation timeout (FR-014); connect/first-byte
// fail-fast is enforced by the HTTP transport (newS3HTTPClient).
func (s *ImageStore) downloadImageS3(ctx context.Context, imageID string, writer io.Writer) error {
	if s.s3Client == nil {
		return fmt.Errorf("S3 client not initialized")
	}

	objectKey := s3ImageKey(imageID)

	result, err := s.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.s3Bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return fmt.Errorf("failed to get object from S3: %w", err)
	}
	defer result.Body.Close()

	_, err = io.Copy(writer, result.Body)
	return err
}

// DeleteImage deletes an image from storage
func (s *ImageStore) DeleteImage(ctx context.Context, imageID string) error {
	switch s.mode {
	case "stub":
		return s.deleteImageStub(imageID)
	case "local":
		return s.deleteImageLocal(ctx, imageID)
	case "rbd":
		return s.deleteImageRBD(ctx, imageID)
	case "s3":
		return s.deleteImageS3(ctx, imageID)
	case "local,rbd":
		// Delete from both (best effort)
		localErr := s.deleteImageLocal(ctx, imageID)
		rbdErr := s.deleteImageRBD(ctx, imageID)
		if localErr != nil && rbdErr != nil {
			return fmt.Errorf("failed to delete: local=%v, rbd=%v", localErr, rbdErr)
		}
		return nil
	case "local,s3":
		// Delete from both (best effort)
		localErr := s.deleteImageLocal(ctx, imageID)
		s3Err := s.deleteImageS3(ctx, imageID)
		if localErr != nil && s3Err != nil {
			return fmt.Errorf("failed to delete: local=%v, s3=%v", localErr, s3Err)
		}
		return nil
	case "rbd,s3":
		// Delete from both (best effort)
		rbdErr := s.deleteImageRBD(ctx, imageID)
		s3Err := s.deleteImageS3(ctx, imageID)
		if rbdErr != nil && s3Err != nil {
			return fmt.Errorf("failed to delete: rbd=%v, s3=%v", rbdErr, s3Err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported storage mode: %s", s.mode)
	}
}

// deleteImageStub simulates image deletion
func (s *ImageStore) deleteImageStub(imageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Delete if exists (idempotent - no error if not found)
	delete(s.stubImages, imageID)
	return nil
}

// deleteImageLocal deletes from local storage
func (s *ImageStore) deleteImageLocal(ctx context.Context, imageID string) error {
	imagePath := filepath.Join(s.localPath, imageFileName(imageID))
	err := os.Remove(imagePath)
	if err != nil && os.IsNotExist(err) {
		// Idempotent delete - not found is success
		return nil
	}
	return err
}

// deleteImageRBD deletes from RBD. This is a stub pending go-ceph
// integration; once wired up, restore the WithTimeout(ctx, s.timeout)
// guard around the exec.CommandContext call.
func (s *ImageStore) deleteImageRBD(_ context.Context, imageID string) error {
	imageName := rbdImageName(imageID)

	// TODO: Use go-ceph
	// ctx, cancel := context.WithTimeout(ctx, s.timeout); defer cancel()
	// cmd := exec.CommandContext(ctx, "rbd", "rm", fmt.Sprintf("%s/%s", s.cephPool, imageName))
	// return cmd.Run()

	return fmt.Errorf("ceph cluster not configured (would delete %s/%s)", s.cephPool, imageName)
}

// deleteImageS3 deletes from S3
func (s *ImageStore) deleteImageS3(ctx context.Context, imageID string) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	if s.s3Client == nil {
		return fmt.Errorf("S3 client not initialized")
	}

	objectKey := s3ImageKey(imageID)

	_, err := s.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.s3Bucket),
		Key:    aws.String(objectKey),
	})

	return err
}

// ImageExists checks if an image exists in storage
func (s *ImageStore) ImageExists(ctx context.Context, imageID string) (bool, error) {
	switch s.mode {
	case "stub":
		s.mu.Lock()
		defer s.mu.Unlock()
		_, exists := s.stubImages[imageID]
		return exists, nil
	case "local":
		return s.imageExistsLocal(imageID)
	case "rbd":
		return s.imageExistsRBD(ctx, imageID)
	case "s3":
		return s.imageExistsS3(ctx, imageID)
	case "local,rbd":
		// Check local first (faster)
		if exists, _ := s.imageExistsLocal(imageID); exists {
			return true, nil
		}
		return s.imageExistsRBD(ctx, imageID)
	case "local,s3":
		// Check local first (faster)
		if exists, _ := s.imageExistsLocal(imageID); exists {
			return true, nil
		}
		return s.imageExistsS3(ctx, imageID)
	case "rbd,s3":
		// Check RBD first
		if exists, _ := s.imageExistsRBD(ctx, imageID); exists {
			return true, nil
		}
		return s.imageExistsS3(ctx, imageID)
	default:
		return false, fmt.Errorf("unsupported storage mode: %s", s.mode)
	}
}

// imageExistsLocal checks if image exists locally
func (s *ImageStore) imageExistsLocal(imageID string) (bool, error) {
	imagePath := filepath.Join(s.localPath, imageFileName(imageID))
	_, err := os.Stat(imagePath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// imageExistsRBD checks if image exists in RBD. This is a stub pending
// go-ceph integration; once wired up, restore the
// WithTimeout(ctx, s.timeout) guard around the exec.CommandContext
// call.
func (s *ImageStore) imageExistsRBD(_ context.Context, imageID string) (bool, error) {
	imageName := rbdImageName(imageID)

	// TODO: Use go-ceph
	// ctx, cancel := context.WithTimeout(ctx, s.timeout); defer cancel()
	// cmd := exec.CommandContext(ctx, "rbd", "info", fmt.Sprintf("%s/%s", s.cephPool, imageName))
	// err := cmd.Run()
	// return err == nil, nil

	return false, fmt.Errorf("ceph cluster not configured (would check %s/%s)", s.cephPool, imageName)
}

// imageExistsS3 checks if image exists in S3
func (s *ImageStore) imageExistsS3(ctx context.Context, imageID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	if s.s3Client == nil {
		return false, fmt.Errorf("S3 client not initialized")
	}

	objectKey := s3ImageKey(imageID)

	_, err := s.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.s3Bucket),
		Key:    aws.String(objectKey),
	})

	if err != nil {
		// A 404 means the object is genuinely absent; any other error (auth,
		// network, wrong endpoint, TLS) MUST surface rather than be masked as
		// "not found" (FR-004).
		var respErr *awshttp.ResponseError
		if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("failed to head S3 object: %w", err)
	}

	return true, nil
}

// GetImageSize returns the size of an image
func (s *ImageStore) GetImageSize(ctx context.Context, imageID string) (int64, error) {
	switch s.mode {
	case "stub":
		s.mu.Lock()
		defer s.mu.Unlock()
		if img, exists := s.stubImages[imageID]; exists {
			return img.size, nil
		}
		return 0, fmt.Errorf("image %s not found", imageID)
	case "local":
		imagePath := filepath.Join(s.localPath, imageFileName(imageID))
		info, err := os.Stat(imagePath)
		if err != nil {
			return 0, err
		}
		return info.Size(), nil
	case "s3":
		return s.getImageSizeS3(ctx, imageID)
	case "local,s3":
		// Try local first (faster), fall back to S3
		localPath := filepath.Join(s.localPath, imageFileName(imageID))
		if info, err := os.Stat(localPath); err == nil {
			return info.Size(), nil
		}
		return s.getImageSizeS3(ctx, imageID)
	case "rbd", "local,rbd", "rbd,s3":
		// For RBD, would parse rbd info output
		return 0, fmt.Errorf("ceph cluster not configured")
	default:
		return 0, fmt.Errorf("unsupported storage mode: %s", s.mode)
	}
}

// getImageSizeS3 returns the size of an image from S3
func (s *ImageStore) getImageSizeS3(ctx context.Context, imageID string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	if s.s3Client == nil {
		return 0, fmt.Errorf("S3 client not initialized")
	}

	objectKey := s3ImageKey(imageID)

	result, err := s.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.s3Bucket),
		Key:    aws.String(objectKey),
	})

	if err != nil {
		return 0, fmt.Errorf("failed to get S3 object metadata: %w", err)
	}

	if result.ContentLength == nil {
		return 0, fmt.Errorf("S3 object has no content length")
	}

	return *result.ContentLength, nil
}

// GetRBDPath returns the RBD path for an image
func (s *ImageStore) GetRBDPath(imageID string) string {
	return fmt.Sprintf("rbd:%s/image-%s", s.cephPool, imageID)
}

// EnsureLocalBase makes sure the raw base for imageID exists at LocalImagePath,
// fetching it from the configured backend (e.g. S3/RGW) and normalizing it to
// raw when absent — the lazy fetch-on-boot path that lets a VM boot from an
// RGW-backed image on a node that holds no local copy (SPEC-014 FR-013). When
// expectedChecksum is non-empty the fetched bytes are verified against it
// (MD5, Glance's checksum) before use, and a mismatch fails rather than booting
// a corrupt image (FR-017). Concurrent callers are serialized per image id so
// the base is written exactly once, atomically. Returns the local base path.
func (s *ImageStore) EnsureLocalBase(ctx context.Context, imageID, expectedChecksum string) (string, error) {
	// s.localPath is LocalImageDir() in production, so this equals
	// LocalImagePath(imageID) — the exact path Nova boots from — while staying
	// overridable in tests.
	finalPath := filepath.Join(s.localPath, imageFileName(imageID))

	// Fast path: already materialized.
	if _, err := os.Stat(finalPath); err == nil {
		return finalPath, nil
	}

	// Serialize per image so simultaneous boots don't race on the base file.
	unlock := s.lockImage(imageID)
	defer unlock()

	// Re-check under the lock — another caller may have just materialized it.
	if _, err := os.Stat(finalPath); err == nil {
		return finalPath, nil
	}

	if err := os.MkdirAll(s.localPath, 0o755); err != nil {
		return "", fmt.Errorf("failed to create image dir %q: %w", s.localPath, err)
	}

	// Download to a temp file while hashing, so integrity is verified before the
	// bytes are ever used to boot a VM.
	tmp, err := os.CreateTemp(s.localPath, imageFileName(imageID)+".fetch-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp fetch file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed into place

	hasher := md5.New() // #nosec G401 -- Glance checksum is MD5 (compatibility), not a security control
	if err := s.DownloadImage(ctx, imageID, io.MultiWriter(tmp, hasher)); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("failed to fetch image %s from backend: %w", imageID, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("failed to close fetched image: %w", err)
	}

	if expectedChecksum != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if got != expectedChecksum {
			return "", fmt.Errorf("image %s checksum mismatch: got %s, want %s", imageID, got, expectedChecksum)
		}
	}

	// Normalize to a raw base at the canonical path (atomic rename when the
	// fetched bytes are already raw).
	if err := normalizeToRaw(tmpPath, finalPath); err != nil {
		_ = os.Remove(finalPath)
		return "", fmt.Errorf("failed to normalize fetched image %s to raw: %w", imageID, err)
	}
	return finalPath, nil
}

// lockImage returns an unlock func for a per-image mutex, serializing
// EnsureLocalBase across concurrent callers for the same image.
func (s *ImageStore) lockImage(imageID string) func() {
	actual, _ := s.fetchLocks.LoadOrStore(imageID, &sync.Mutex{})
	mu := actual.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
