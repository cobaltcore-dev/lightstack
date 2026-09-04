# Phase 0 Research — Ceph RGW (S3) integration

Resolves every deferred/unknown decision from the spec (incl. the `/cl.plan`-deferred items) and grounds each in the current code. Format: **Decision / Rationale / Alternatives**.

## 1. RGW↔lightstack trust mechanism (the deferred decision) — `/v3/s3tokens` is public + self-validating

- **Decision**: Implement `POST /v3/s3tokens` (and `/v3/ec2tokens`) as a **public, unauthenticated** endpoint on Keystone's `v3` group. It validates the request by looking up the EC2 credential by access key and **recomputing the S3 signature** from the stored secret; on success it returns the resolved user/project/roles. RGW is configured with `rgw s3 auth use keystone = true` + `rgw keystone url = <lightstack>` + `rgw keystone api version = 3`; **no Lightstack admin/service token is required** from RGW.
- **Rationale**: This is exactly how OpenStack Keystone works — the S3 signature *is* the credential, so the endpoint self-validates and needs no caller auth. Grounded: Lightstack has **no service/admin-token concept** today (only user JWT via `X-Auth-Token`; `internal/middleware/auth.go`), and the public-vs-authenticated split is by route group (`v3` vs `mgmt`) in `internal/keystone/handlers.go:33-124`. Registering `s3tokens` on the `v3` group cleanly bypasses `authMiddleware`.
- **Alternatives rejected**: (a) *Static `rgw keystone admin token`* — Lightstack has no static-token type; adds config + a shared secret for no security gain over signature self-validation. (b) *Admin service-user* — RGW would need to hold/refresh a real Lightstack admin **user** JWT; not how RGW's keystone integration authenticates s3token calls, and heavier.

## 2. EC2-credentials storage — reuse `credentials` table + an `access_key` lookup column

- **Decision**: Store EC2 credentials in the existing `credentials` table (`migrations/041`) as `type='ec2'`, `blob='{"access":..,"secret":..,"tenant_id":..,"user_id":..}'`. Add a nullable, **unique `access_key` column** (+ index) in migration `062` so `/v3/s3tokens` can resolve access-key → credential in one indexed query. Generate the access/secret pair with `crypto/rand` (mirroring `application_credentials.go:118-121`).
- **Rationale**: The generic `(type, blob)` model is already OpenStack's EC2-credential shape; a contract test already POSTs `type:"ec2"`. The only missing piece is a fast, unique lookup by access key — `credentials` is indexed only by user/project/type today.
- **Alternatives rejected**: (a) *JSON expression index on `blob`* — works but is opaque and can't easily enforce access-key uniqueness. (b) *New dedicated `ec2_credentials` table* — duplicates the generic model and the existing CRUD in `credentials.go`.
- **Secret storage**: stored **recoverable** (plaintext in `blob`), NOT bcrypt — required to recompute the HMAC signature (see plan Complexity Tracking). Confined to `blob`; protect via DB access control; document.

## 3. S3 signature verification — recompute SigV4 (and SigV2) in-process, stdlib only

- **Decision**: Verify AWS **SigV4** primarily (the aws-sdk-go-v2 default RGW clients use), with **SigV2** as a fallback for older clients. Recompute using stdlib `crypto/hmac` + `crypto/sha256` from the `s3tokens` request body (RGW forwards the string-to-sign / canonical request + the client's signature + access key). Compare in constant time.
- **Rationale**: No server-side signature *verification* exists in Lightstack today (only client-side signing via aws-sdk). stdlib HMAC/SHA-256 is sufficient; no new dependency. The `s3tokens` body format is defined by Keystone/RGW (see `contracts/s3tokens.md`).
- **Alternatives rejected**: Pulling a third-party SigV4 *verifier* — unnecessary; the algorithm is small and stdlib-friendly, and avoids a dependency (Constitution: minimal deps).

## 4. Per-project isolation mechanism — RGW `implicit tenants = s3`

- **Decision**: Configure RGW with `rgw keystone implicit tenants = s3`, mapping each Lightstack project to its own RGW tenant → per-project bucket namespace (FR-008). Cross-tenant access requires explicit `tenant:bucket` addressing + permission. lightstack's `s3tokens` response returns the project id as the tenant so RGW namespaces correctly.
- **Rationale**: This reproduces the OpenStack/Swift `AUTH_<project>` account model (confirmed via Ceph reef docs during clarification). It's RGW-side config, not Lightstack code — Lightstack only needs to return the correct project/tenant in the s3token response.
- **Alternatives rejected**: Shared global namespace (AWS-S3 default) — breaks multi-tenant isolation expectations; rejected in clarification.

## 5. Lazy fetch-on-boot seam — `storage.EnsureLocalBase` + Nova gets a `*storage.ImageStore`

- **Decision**: Add `storage.EnsureLocalBase(ctx, imageID, expectedChecksum) (string, error)` in `pkg/storage` that: returns the path if the local raw base exists; else downloads `images/image-<id>.raw` from the backend (S3), **normalizes to raw** (reusing the private `normalizeToRaw`), **verifies the checksum**, and atomically renames into place. Thread the Glance storage config into `nova.NewService` so Nova builds a `*storage.ImageStore` and calls `EnsureLocalBase` in the real-mode boot branch (`internal/nova/handlers.go` ~line 588, immediately before `hypervisor.StageOverlay`).
- **Rationale**: Nova already imports `pkg/storage` and boots from `storage.LocalImagePath`; it just lacks the materialization step. Keeping fetch+normalize+verify inside `pkg/storage` avoids exporting `normalizeToRaw`/`detectDiskFormat` and keeps Glance's write naming and Nova's read naming in lockstep (the file's stated "single source of truth", `image_store.go:43-45`).
- **Alternatives rejected**: (a) *Fetch via Glance's HTTP `GET /v2/images/:id/file`* — adds an HTTP hop + token handling; Nova already has the in-process seam. (b) *Write-through cache on upload* (`local,s3`) — only caches on the upload node, not multi-node (rejected as FR-013 option B in clarification).

## 6. Checksum integrity (prerequisite for FR-017) — write `images.checksum` on upload

- **Decision**: Compute a SHA-256 (or MD5 for Glance-compat `checksum` field) over the **normalized raw** bytes during upload and persist it to the existing-but-unwritten `images.checksum` column; `EnsureLocalBase` verifies the fetched base against it and fails the boot on mismatch (FR-017).
- **Rationale**: **Grounded gap** — `images.checksum` exists (`migrations/001`) but is **never written** (`CreateImage`/`UploadImageData` omit it). Without a stored digest there is nothing to verify against, so checksum-on-upload is a hard prerequisite. Glance's `checksum` is conventionally MD5; `os_hash_value`/`os_hash_algo` (SHA-512) is the modern field — plan will store MD5 in `checksum` for client-compat and may add `os_hash_*` if present. (Confirm columns in data-model.md.)
- **Alternatives rejected**: Verify against S3 ETag — ETag is not stable for multipart uploads and isn't the Glance-visible digest; use Glance's own checksum.

## 7. Node-local cache eviction (FR-016) — cluster-wide refcount now; per-node needs a future `host` column

- **Decision**: A reclamation pass deletes `/var/lib/lightstack/images/image-<id>.raw` when **no** row references the image: `SELECT 1 FROM instances WHERE image_id=$1 UNION ALL SELECT 1 FROM volumes WHERE source_image_id=$1` (the exact predicate Glance's `DeleteImage` already uses, `internal/glance/images.go:462-477`). Trigger: on instance delete and/or a periodic sweep.
- **Rationale**: Instance deletes are **hard deletes** (no `DELETED` rows), so the refcount is accurate. Lightstack is effectively single-node (all state under `/var/lib/lightstack`; **no `host`/`node_id` column on `instances`**), so cluster-wide == node-local today. On a genuine multi-node cluster this refcount is *conservative* (a node retains a base while the image is used anywhere) — safe, never evicts an in-use base, just less aggressive.
- **Limitation recorded**: precise per-node eviction needs a `host` column on `instances` (set on create, filtered in the refcount) — **out of scope**, future work. Note: the existing Glance image-cache handlers (`internal/glance/cache.go`, `images.cached_at`) are a *separate* mechanism and are not reused here.
- **Alternatives rejected**: LRU/size-cap without refcount — risks evicting a base a stopped-but-restartable VM still backs onto.

## 8. Concurrent same-image fetch (#4, plan-level) — per-image lock + atomic rename

- **Decision**: Serialize `EnsureLocalBase` per image id (in-process `sync.Map` of mutexes keyed by image id) and download to a temp file + `os.Rename` into the final path (rename is atomic on the same filesystem). A second concurrent boot either waits and finds the base present, or shares the completed base.
- **Rationale**: Two simultaneous boots of the same image on one node would otherwise race on the base file. The upload path already uses a temp-file+rename idiom (`uploadImageLocal` → `.upload` temp; `image_store.go:253`), so this is consistent.
- **Alternatives rejected**: File-lock (flock) — heavier; in-process is sufficient for a single-binary node. No lock — risks a half-written base.

## 9. TLS / custom CA + timeout rework — inject a buildable aws http client

- **Decision**: Build a custom client via `awshttp.NewBuildableClient().WithTransportOptions(...)` and pass it with `config.WithHTTPClient`. Set `TLSClientConfig{RootCAs: pool, InsecureSkipVerify: <test-only>}` (pool from `x509.NewCertPool()` + `AppendCertsFromPEM(os.ReadFile(caBundle))`). Set transport **connect** (`Dialer.Timeout`) and **first-byte** (`Transport.ResponseHeaderTimeout`) budgets short (fail-fast, ~1–5s configurable), and **stop wrapping the body copy** in `s.timeout` — copy `GetObject`/`PutObject` bodies under the request context so transfers stream to completion (FR-014).
- **Rationale**: Grounded — `initS3Client` currently sets only region/endpoint/path-style and applies a single 5s `context.WithTimeout` around the whole download incl. `io.Copy(writer, result.Body)` (`image_store.go:491-512`), which cancels large transfers. `ResponseHeaderTimeout` (default unset) is the precise "first-byte" knob. New config fields: `s3_ca_bundle`, `s3_insecure_skip_verify` (+ optional connect/first-byte timeout); HTTP stays for dev/CI.
- **Alternatives rejected**: `http.Client.Timeout` / `BuildableClient.WithTimeout` — whole-request timeout, bounds the body too (wrong). Global `InsecureSkipVerify` default — forbidden (FR-015: test-only, never default).

## 10. Catalog `s3` entry — migration `062` + hardcoded fallback

- **Decision**: Migration `062` inserts a `services` row `type='s3'` (name e.g. `rgw`) and an `endpoints` row (interface `public`, region set, url = RGW S3 endpoint). Mirror an `s3` entry in `buildHardcodedCatalog` (`auth.go:675-728`). The catalog entry is for discovery/completeness (S3 clients configure the endpoint directly); FR-009.
- **Rationale**: Catalog is seeded purely in SQL migrations (no `lightstack seed`); next free number is `062` (latest `061`); `ValidateMigrations` requires paired up/down. Types today: identity/compute/network/volumev3/image (+ placement fallback).
- **Alternatives rejected**: `object-store` (Swift) type — misleads Swift clients (no Swift API); rejected in clarification (FR-009 uses `s3`).

## 11. CI RGW fixture + s3tokens loopback — `ceph/demo` in docker-compose

- **Decision**: A CI job runs `ceph/demo` (all-in-one real RGW) + Lightstack + postgres in one docker-compose network; a bash driver validates (A) Glance upload/download/checksum + boot-fetch and (B) EC2-creds → S3 client (runner-as-VM) → RGW → Lightstack `/v3/s3tokens`. RGW reaches Lightstack over the compose network (both containers); the runner reaches RGW via a published port.
- **Rationale**: Rook/k8s is impractical in CI (established in clarification). `ceph/demo` gives real RGW quirks. The s3tokens loopback (RGW→lightstack) works because both are on the same docker network. The "tenant" is the CI runner acting as an S3 client (no guest VM; FR gate = auth chain).
- **Alternatives rejected**: MinIO (not RGW — no keystone/implicit-tenants), two k8s clusters (infeasible on runners).

## 12. Config threading — options struct over positional bloat

- **Decision**: Introduce a small `storage.S3Options`/`glance` config struct to carry the growing S3 settings (bucket/region/endpoint/ca-bundle/insecure/timeouts) instead of extending the positional `NewImageStore`/`glance.NewService` signatures further.
- **Rationale**: Grounded — S3 settings are passed positionally through `main.go → glance.NewService → storage.NewImageStore` and duplicated on the `glance.Service` struct; each new field currently touches 3 signatures. An options struct keeps it maintainable.
- **Alternatives rejected**: Continue positional — signature churn and error-prone ordering.

## Resolved unknowns summary

All Technical-Context items are resolved (no remaining NEEDS CLARIFICATION). Open, explicitly-deferred **future** items (out of scope, recorded): per-node cache eviction via a `host` column; SigV2-only ancient clients beyond the fallback; Swift API (lightstack#15).
