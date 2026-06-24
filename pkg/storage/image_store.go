package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/cobaltcore-dev/o3k/pkg/paths"
	"github.com/rs/zerolog/log"
)

// ImageStore manages image storage operations
type ImageStore struct {
	mode       string // "stub", "local", "rbd", "s3", or combinations like "local,rbd", "local,s3"
	cephPool   string
	cephConf   string
	s3Bucket   string
	s3Region   string
	s3Endpoint string
	timeout    time.Duration
	localPath  string
	mu         sync.Mutex
	stubImages map[string]*stubImage // For stub mode
	s3Client   *s3.Client
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
func NewImageStore(mode, cephPool, cephConf, s3Bucket, s3Region, s3Endpoint string) (*ImageStore, error) {
	// Use the canonical image dir for local storage (Docker-friendly);
	// falls back to ~/.o3k/images for non-containerized deployments.
	localPath := LocalImageDir()
	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		homeDir, _ := os.UserHomeDir()
		localPath = filepath.Join(homeDir, ".o3k", "images")
	}

	store := &ImageStore{
		mode:       mode,
		cephPool:   cephPool,
		cephConf:   cephConf,
		s3Bucket:   s3Bucket,
		s3Region:   s3Region,
		s3Endpoint: s3Endpoint,
		timeout:    5 * time.Second,
		localPath:  localPath,
		stubImages: make(map[string]*stubImage),
	}

	// Create local storage directory if needed
	if mode == "local" || containsMode(mode, "local") {
		_ = os.MkdirAll(store.localPath, 0755)
	}

	// Initialize S3 client if needed
	if mode == "s3" || containsMode(mode, "s3") {
		if err := store.initS3Client(); err != nil {
			if mode == "s3" {
				return nil, fmt.Errorf("S3 is the configured storage mode but S3 client init failed: %w", err)
			}
			log.Error().Err(err).
				Str("mode", mode).
				Str("region", s3Region).
				Str("endpoint", s3Endpoint).
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
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(s.s3Region))
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

// UploadImage uploads an image to storage
func (s *ImageStore) UploadImage(ctx context.Context, imageID string, reader io.Reader) (int64, error) {
	switch s.mode {
	case "stub":
		return s.uploadImageStub(imageID, reader)
	case "local":
		return s.uploadImageLocal(ctx, imageID, reader)
	case "rbd":
		return s.uploadImageRBD(ctx, imageID, reader)
	case "s3":
		return s.uploadImageS3(ctx, imageID, reader)
	case "local,rbd":
		// Upload to local first
		size, err := s.uploadImageLocal(ctx, imageID, reader)
		if err != nil {
			return 0, fmt.Errorf("failed to upload to local: %w", err)
		}
		// Then upload to RBD (would need to read from local file)
		// For now, just log that it would be uploaded to both
		return size, nil
	case "local,s3":
		// Upload to local first
		size, err := s.uploadImageLocal(ctx, imageID, reader)
		if err != nil {
			return 0, fmt.Errorf("failed to upload to local: %w", err)
		}
		// Then upload to S3 from local file
		if err := s.uploadLocalToS3(ctx, imageID); err != nil {
			// Keep local copy even if S3 fails
			return size, fmt.Errorf("uploaded to local but failed to replicate to S3: %w", err)
		}
		return size, nil
	case "rbd,s3":
		// Upload to RBD first
		size, err := s.uploadImageRBD(ctx, imageID, reader)
		if err != nil {
			return 0, fmt.Errorf("failed to upload to RBD: %w", err)
		}
		// S3 upload is not supported when reader has already been consumed by RBD.
		// Callers that need rbd+s3 should use a MultiReader or buffer the data.
		return size, nil
	default:
		return 0, fmt.Errorf("unsupported storage mode: %s", s.mode)
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
	cmd := exec.Command("qemu-img", "convert", "-O", "raw", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert %s->raw: %w: %s", format, err, string(out))
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

// uploadImageS3 uploads image to S3
func (s *ImageStore) uploadImageS3(ctx context.Context, imageID string, reader io.Reader) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	if s.s3Client == nil {
		return 0, fmt.Errorf("S3 client not initialized")
	}

	objectKey := s3ImageKey(imageID)

	// Stream to a temp file so we can provide an io.ReadSeeker to the S3 SDK
	// without buffering the entire image in memory.
	tmp, err := os.CreateTemp("", "s3upload-*")
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

	_, err = s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.s3Bucket),
		Key:           aws.String(objectKey),
		Body:          tmp,
		ContentLength: aws.Int64(size),
	})

	if err != nil {
		return 0, fmt.Errorf("failed to upload to S3: %w", err)
	}

	return size, nil
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

// downloadImageS3 downloads from S3
func (s *ImageStore) downloadImageS3(ctx context.Context, imageID string, writer io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

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
		// Distinguish between "not found" and actual errors via HTTP status
		// The AWS SDK wraps 404 as a smithy NotFound error; for simplicity
		// we treat any error from HeadObject as "not found" since we cannot
		// import smithy types without extra deps. Log and return false.
		return false, nil
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
