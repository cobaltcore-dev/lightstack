package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// Service handles EC2-compatible metadata service endpoints
type Service struct {
	// In production, this would run on 169.254.169.254
	// For testing, it runs on localhost:8775
	bindAddr string
	pool     database.DB

	// trustInstanceHeader believes X-Instance-ID from any caller
	// (metadata.trust_instance_header). Test stacks only.
	trustInstanceHeader bool
}

// NewService creates a new metadata service
func NewService(pool database.DB, bindAddr string) *Service {
	return &Service{
		bindAddr: bindAddr,
		pool:     pool,
	}
}

// SetTrustInstanceHeader makes X-Instance-ID believed from any caller, not only
// loopback. It exists for test stacks whose requests arrive from elsewhere, such as a
// Docker bridge gateway, and must stay off wherever real guests reach this service:
// with it on, any guest can read any instance's metadata and user-data.
func (svc *Service) SetTrustInstanceHeader(trust bool) {
	svc.trustInstanceHeader = trust
}

// BindAddr returns the configured listen address for the metadata server.
func (svc *Service) BindAddr() string {
	return svc.bindAddr
}

// RegisterRoutes registers metadata service routes
func (svc *Service) RegisterRoutes(r *gin.RouterGroup) {
	// EC2-compatible metadata API (OpenStack follows this format)
	// Cloud-init tries these paths in order:
	//   /openstack/latest/meta_data.json
	//   /2009-04-04/meta-data/

	// OpenStack-style metadata
	openstack := r.Group("/openstack")
	{
		openstack.GET("/latest/meta_data.json", svc.GetMetaDataJSON)
		openstack.GET("/latest/user_data", svc.GetUserData)
		openstack.GET("/latest/network_data.json", svc.GetNetworkDataJSON)
		openstack.GET("/latest/vendor_data.json", svc.GetVendorDataJSON)
	}

	// EC2-style metadata (the datasource cirros and most cloud images use). A
	// wildcard handles the public-keys subtree (…/public-keys/0/openssh-key) that
	// injects the SSH key. Registered under both the dated version and "latest".
	for _, ver := range []string{"/2009-04-04", "/latest"} {
		ec2 := r.Group(ver)
		ec2.GET("/meta-data/*path", svc.GetEC2MetaData)
		ec2.GET("/user-data", svc.GetUserData)
	}

	// Version discovery
	r.GET("/", svc.ListVersions)
}

// instanceFromRequest identifies the calling guest by the TCP peer address of its
// request: the fixed IP of one of its ports, which the per-bridge 169.254.169.254 DNAT
// preserves. Nothing the caller sends can change that answer. Upstream Nova believes
// X-Forwarded-For only behind its own metadata proxy (use_forwarded_for, off by
// default) and X-Instance-ID only with that proxy's HMAC signature. No proxy stands in
// front of this server, so gin's ClientIP, which believes X-Forwarded-For from anyone,
// would let a guest answer as any address it could name.
func (svc *Service) instanceFromRequest(c *gin.Context) (string, error) {
	peer := c.RemoteIP()

	// X-Instance-ID names the instance directly, for callers outside any guest: the unit
	// and contract tests. Believed from loopback, or everywhere once a test stack turned
	// on metadata.trust_instance_header. From any other caller it is ignored, and the
	// caller is identified by its address like every guest.
	if instanceID := c.GetHeader("X-Instance-ID"); instanceID != "" && svc.trustsInstanceHeader(peer) {
		return instanceID, nil
	}

	if net.ParseIP(peer) != nil {
		// ports stores device_id (the attached instance) and fixed_ips as a JSONB
		// array of {subnet_id, ip_address}; match the source IP against it.
		//
		// SPEC-009 v1 LIMITATION (single-node, #503): identity is matched by source
		// IP alone, NOT scoped to the request's network. With non-overlapping tenant
		// CIDRs the fixed IP is unique, so this is correct. Two tenants sharing a CIDR
		// (e.g. both 10.0.0.0/24, each with a VM at 10.0.0.5) would cross-identify
		// here — but that case is already unsupported at L2 in the flat single-node
		// model (bridges share the gateway IP), so it is deferred to multi-node v2
		// (#504), where per-tenant network namespaces + network-scoped metadata
		// identity belong.
		//
		// The text search only narrows the ports down to those whose fixed_ips mention
		// the address as a whole JSON string; the address itself is compared below.
		instanceID, err := svc.instanceHolding(c.Request.Context(), peer)
		if err != nil {
			return "", err // a database failure is a 500, not "no such instance"
		}
		if instanceID != "" {
			return instanceID, nil
		}
	}

	return "", errUnidentified
}

// errUnidentified: the caller could not be matched to an instance — a 404, as opposed to a
// database failure while looking it up, which is a 500.
var errUnidentified = errors.New("cannot determine instance ID from request")

// writeIdentifyError answers an instanceFromRequest failure: 404 for a caller that cannot be
// identified, 500 for a database error. plain selects the empty-body text form the EC2
// endpoints use rather than the JSON error envelope.
func writeIdentifyError(c *gin.Context, err error, plain bool) {
	if errors.Is(err, errUnidentified) {
		if plain {
			c.String(http.StatusNotFound, "")
		} else {
			common.SendError(c, common.NewNotFoundError("instance"))
		}
		return
	}
	log.Warn().Err(err).Msg("metadata: identifying the instance failed")
	if plain {
		c.String(http.StatusInternalServerError, "")
	} else {
		common.SendError(c, common.NewInternalServerError("failed to identify the instance"))
	}
}

// instanceHolding returns the instance whose port holds ip as a fixed IP, or "" when none
// does. A database failure is returned as an error, so the caller answers 500 rather than a
// 404 that tells the guest it has no instance.
//
// The port must belong to the same project as the instance it names. A port's device_id
// is caller-settable (PUT /v2.0/ports/{id}), so without the join a tenant could point
// their own port at another project's instance and read its metadata and user-data; the
// join drops that row, because the port carries the caller's project, not the victim's.
// Two different instances holding one address is ambiguous and resolves to "" (upstream's
// metadata agent answers only when exactly one port matches).
func (svc *Service) instanceHolding(ctx context.Context, ip string) (string, error) {
	rows, err := svc.pool.Query(ctx,
		`SELECT p.device_id, p.fixed_ips FROM ports p
		 JOIN instances i ON i.id = p.device_id AND i.project_id = p.project_id
		 WHERE p.fixed_ips::text LIKE '%' || $1 || '%'`,
		strconv.Quote(ip))
	if err != nil {
		return "", fmt.Errorf("looking up the instance holding %s: %w", ip, err)
	}
	defer rows.Close()
	holder := ""
	for rows.Next() {
		var deviceID string
		var fixedIPs common.FixedIPs
		if err := rows.Scan(&deviceID, &fixedIPs); err != nil {
			// A single undecodable row must not hide a later exact match: log and skip it.
			log.Warn().Err(err).Str("ip", ip).Msg("metadata: reading a port failed; skipping it")
			continue
		}
		if deviceID == "" || !fixedIPs.Holds(ip) {
			continue
		}
		if holder != "" && holder != deviceID {
			log.Warn().Str("ip", ip).Msg("metadata: address held by more than one instance; not identifying")
			return "", nil
		}
		holder = deviceID
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("looking up the instance holding %s: %w", ip, err)
	}
	return holder, nil
}

// trustsInstanceHeader reports whether a request from peer may name its instance with
// X-Instance-ID.
func (svc *Service) trustsInstanceHeader(peer string) bool {
	if svc.trustInstanceHeader {
		return true
	}
	ip := net.ParseIP(peer)
	return ip != nil && ip.IsLoopback()
}

// GetMetaDataJSON returns instance metadata in JSON format (OpenStack style)
func (svc *Service) GetMetaDataJSON(c *gin.Context) {
	instanceID, err := svc.instanceFromRequest(c)
	if err != nil {
		writeIdentifyError(c, err, false)
		return
	}

	var name, hostname, projectID, userID string
	var uuid string
	var keyName sql.NullString
	err = svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, name, project_id, user_id, key_name
		FROM instances
		WHERE id = $1
	`, instanceID).Scan(&uuid, &name, &hostname, &projectID, &userID, &keyName)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_metadata_json").Str("instance_id", instanceID).Msg("failed to query instance")
		common.SendError(c, common.NewInternalServerError("failed to get instance metadata"))
		return
	}

	// Build metadata structure
	metadata := gin.H{
		"uuid":              uuid,
		"name":              name,
		"hostname":          hostname,
		"project_id":        projectID,
		"availability_zone": defaultAvailabilityZone,
		"launch_index":      0,
		"meta":              gin.H{}, // Custom metadata key-value pairs
		"public_keys":       gin.H{}, // SSH keys
	}

	// Fetch custom metadata. A failed query fails the request like the reads below
	// it: swallowing it served every guest an empty `meta`.
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT meta_key, meta_value
		FROM instance_metadata
		WHERE instance_id = $1
	`, instanceID)
	if err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to query instance metadata")
		common.SendError(c, common.NewInternalServerError("failed to get instance metadata"))
		return
	}
	defer rows.Close()
	metaMap := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to scan instance metadata row")
			common.SendError(c, common.NewInternalServerError("failed to get instance metadata"))
			return
		}
		metaMap[key] = value
	}
	// Fail rather than serve silently-incomplete instance metadata; cloud-init
	// retries, whereas a missing key would look authoritative.
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Msg("instance metadata rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to get instance metadata"))
		return
	}
	if len(metaMap) > 0 {
		metadata["meta"] = metaMap
	}

	// Fetch the SSH key the instance was booted with. Upstream Nova keys this off
	// instance.key_name and omits public_keys entirely when there is none, rather than
	// injecting every key the owner happens to have. No key_name → no query: the
	// response keeps the empty public_keys it already defaults to.
	if keyName.Valid && keyName.String != "" {
		keyRows, err := svc.pool.Query(c.Request.Context(), `
			SELECT k.name, k.public_key
			FROM keypairs k
			WHERE k.user_id = $1 AND k.name = $2
		`, userID, keyName.String)
		if err == nil {
			defer keyRows.Close()
			keysMap := make(map[string]string)
			for keyRows.Next() {
				var name, pubkey string
				if err := keyRows.Scan(&name, &pubkey); err != nil {
					log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to scan public key row")
					common.SendError(c, common.NewInternalServerError("failed to get instance metadata"))
					return
				}
				keysMap[name] = pubkey
			}
			// Fail closed on a truncated key read: serving an incomplete public_keys map
			// boots the guest without its SSH key and locks the user out. cloud-init retries
			// on error. Matches publicKeysForInstance on the EC2 metadata path.
			if err := keyRows.Err(); err != nil {
				log.Error().Err(err).Str("instance_id", instanceID).Msg("public key rows iteration error")
				common.SendError(c, common.NewInternalServerError("failed to get instance metadata"))
				return
			}
			if len(keysMap) > 0 {
				metadata["public_keys"] = keysMap
			}
		}
	}

	c.JSON(http.StatusOK, metadata)
}

// GetUserData returns cloud-init user-data
func (svc *Service) GetUserData(c *gin.Context) {
	instanceID, err := svc.instanceFromRequest(c)
	if err != nil {
		writeIdentifyError(c, err, true)
		return
	}

	var userData string
	err = svc.pool.QueryRow(c.Request.Context(), `
		SELECT userdata
		FROM instance_userdata
		WHERE instance_id = $1
	`, instanceID).Scan(&userData)

	if errors.Is(err, database.ErrNoRows) {
		// No user-data is not an error, just return empty
		c.String(http.StatusOK, "")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "")
		return
	}

	// User-data can be shell script, cloud-config, etc.
	c.String(http.StatusOK, userData)
}

// GetNetworkDataJSON returns network configuration in JSON format
func (svc *Service) GetNetworkDataJSON(c *gin.Context) {
	instanceID, err := svc.instanceFromRequest(c)
	if err != nil {
		writeIdentifyError(c, err, false)
		return
	}

	// Query all ports for this instance
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT p.id, p.mac_address, p.fixed_ip, p.network_id, n.name, s.cidr, s.gateway_ip, s.dns_nameservers
		FROM ports p
		JOIN networks n ON p.network_id = n.id
		LEFT JOIN subnets s ON p.network_id = s.network_id
		WHERE p.device_id = $1
		ORDER BY p.created_at
	`, instanceID)

	if err != nil {
		log.Error().Err(err).Str("operation", "get_network_data_json").Str("instance_id", instanceID).Msg("failed to query instance ports")
		common.SendError(c, common.NewInternalServerError("failed to get network data"))
		return
	}
	defer rows.Close()

	var links []gin.H
	var networks []gin.H

	linkID := 0
	for rows.Next() {
		var portID, mac, ip, networkID, networkName, cidr, gateway string
		var dnsServers []string

		if err := rows.Scan(&portID, &mac, &ip, &networkID, &networkName, &cidr, &gateway, &dnsServers); err != nil {
			log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to scan network data row")
			common.SendError(c, common.NewInternalServerError("failed to get network data"))
			return
		}

		// Create link (interface)
		links = append(links, gin.H{
			"id":                   fmt.Sprintf("tap%d", linkID),
			"type":                 "bridge",
			"ethernet_mac_address": mac,
			"mtu":                  1500,
		})

		// Create network config
		netConfig := gin.H{
			"id":         fmt.Sprintf("network%d", linkID),
			"link":       fmt.Sprintf("tap%d", linkID),
			"type":       "ipv4",
			"ip_address": ip,
			"netmask":    cidrToNetmask(cidr), // Convert /24 → "255.255.255.0"
		}

		if gateway != "" {
			netConfig["gateway"] = gateway
		}

		if len(dnsServers) > 0 {
			netConfig["dns_nameservers"] = dnsServers
		}

		networks = append(networks, netConfig)
		linkID++
	}
	// Fail rather than serve a partial network_data.json: a missing interface would
	// leave the guest misconfigured, and cloud-init retries on error.
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("network data rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to get network data"))
		return
	}

	// Return network_data.json structure
	c.JSON(http.StatusOK, gin.H{
		"links":    links,
		"networks": networks,
		"services": []gin.H{},
	})
}

// GetVendorDataJSON returns vendor-specific data (usually empty)
func (svc *Service) GetVendorDataJSON(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{})
}

// ec2InstanceID is the instance-id the EC2 tree reports: "i-" and the first 17 hex
// digits of the instance UUID, AWS's current id length. Guests check the prefix —
// cirros's EC2 datasource retries until it sees "i-" — and 17 digits keep ids unique in
// practice: about 62 random bits of a v4 UUID. Upstream Nova serves i-%08x from a
// per-UUID counter table; deriving the id needs no table, and it starts with the same
// 8 digits as the libvirt domain name. meta_data.json keeps the full UUID as uuid.
func ec2InstanceID(instanceID string) string {
	digits := strings.ToLower(strings.ReplaceAll(instanceID, "-", ""))
	if len(digits) > 17 {
		digits = digits[:17]
	}
	return "i-" + digits
}

// defaultAvailabilityZone and rootBlockDevice are the single-node deployment's fixed answers. Named
// rather than inlined so the EC2 tree and the OpenStack JSON view cannot drift apart.
const (
	defaultAvailabilityZone = "nova"
	rootBlockDevice         = "vda"
)

// ec2Request is what one node of the EC2 tree needs to answer: the service, the request, and the
// instance already resolved from its source address. Passed as a value so a handler is a plain
// func(ec2Request) and the tree below can be a package-level map rather than closures rebuilt per
// request.
//
// The context is derived on demand rather than stored, so nothing holds a context in a struct.
type ec2Request struct {
	svc        *Service
	c          *gin.Context
	instanceID string
}

func (r ec2Request) ctx() context.Context { return r.c.Request.Context() }
func (r ec2Request) ok(body string)       { r.c.String(http.StatusOK, body) }
func (r ec2Request) notFound()            { r.c.String(http.StatusNotFound, "") }

// serverError reports a read that failed, as distinct from a key that is absent. Serving a short
// list would boot the guest without its SSH key and lock the user out; an error makes cloud-init
// retry (the rule #549 established for every metadata read).
func (r ec2Request) serverError() { r.c.String(http.StatusInternalServerError, "") }

// ec2Entry is one node of the EC2 metadata tree.
type ec2Entry struct {
	// dir marks a subtree. EC2 lists those with a trailing slash, which is how a datasource knows to
	// descend rather than read.
	dir bool
	// hidden serves a key without advertising it. Only public-ipv4 uses this: a guest with no
	// floating IP genuinely has no public address, and listing a key that then 404s is the exact
	// thing the listing exists to prevent.
	hidden bool
	serve  func(ec2Request)
}

// ec2Tree maps a metadata path to the node that answers it.
//
// A map rather than a switch because the root listing is DERIVED from it (buildEC2RootListing).
// Previously the listing was a hand-maintained slice that a comment asked you to keep in step with
// the cases below it, and the test named for that invariant restated the same literals a third time
// without ever fetching them — so nothing actually enforced it. Now a key cannot be served without
// being listed, or listed without being served, because there is one description of both.
//
// Sub-paths carry a "/" in their key and are therefore never top-level listed; the parent's own
// entry enumerates them. Handlers are shared where the answer is the same (three names for the
// hostname, two for the boot disk) instead of the bodies being repeated.
var ec2Tree = map[string]ec2Entry{
	"instance-id":      {serve: ec2InstanceIDKey},
	"hostname":         {serve: ec2Hostname},
	"local-hostname":   {serve: ec2Hostname},
	"public-hostname":  {serve: ec2Hostname},
	"instance-type":    {serve: ec2InstanceType},
	"local-ipv4":       {serve: ec2LocalIPv4},
	"ami-launch-index": {serve: ec2AMILaunchIndex},
	"public-ipv4":      {hidden: true, serve: ec2PublicIPv4},

	"placement":                   {dir: true, serve: ec2Placement},
	"placement/availability-zone": {serve: ec2AvailabilityZone},

	"block-device-mapping":      {dir: true, serve: ec2BlockDeviceMapping},
	"block-device-mapping/ami":  {serve: ec2BootDisk},
	"block-device-mapping/root": {serve: ec2BootDisk},

	// The indexed children (public-keys/<n>[/openssh-key]) depend on how many keys the instance has,
	// so they cannot be map keys and are handled by the prefix branch in GetEC2MetaData.
	"public-keys": {dir: true, serve: ec2PublicKeyIndex},
}

// ec2RootListing is the response for the tree root, built once at init from ec2Tree.
var ec2RootListing = buildEC2RootListing()

// buildEC2RootListing renders the top-level entries: every non-hidden key without a "/", a trailing
// slash on the subtrees, sorted so the output is stable.
//
// It also rejects a node with no handler. That can only come from an editing mistake, and the
// alternatives are both bad: dispatching to it panics on a nil func, while skipping it would leave a
// key advertised by this very listing and then answering 404 — the mismatch the listing exists to
// prevent. Failing at init means such a tree cannot start, let alone ship. No unit test asserts this,
// deliberately: the package would not load for the test to run.
func buildEC2RootListing() string {
	names := make([]string, 0, len(ec2Tree))
	for path, entry := range ec2Tree {
		if entry.serve == nil {
			panic("ec2Tree: entry " + path + " has no serve func")
		}
		if entry.hidden || strings.Contains(path, "/") {
			continue
		}
		if entry.dir {
			path += "/"
		}
		names = append(names, path)
	}
	sort.Strings(names)
	return strings.Join(names, "\n")
}

// ec2InstanceIDKey serves instance-id. Not named ec2InstanceID: that is the derivation, a plain
// string function the identity tests also call.
func ec2InstanceIDKey(r ec2Request) { r.ok(ec2InstanceID(r.instanceID)) }

// ec2Hostname answers hostname, local-hostname and public-hostname alike. Lightstack models no separate
// public DNS name, and returning the same value is what a flat single-node deployment means — not a
// placeholder.
func ec2Hostname(r ec2Request) {
	var hostname string
	if err := r.svc.pool.QueryRow(r.ctx(),
		`SELECT name FROM instances WHERE id = $1`, r.instanceID).Scan(&hostname); err != nil {
		r.notFound()
		return
	}
	r.ok(hostname)
}

func ec2InstanceType(r ec2Request) {
	var flavorName string
	if err := r.svc.pool.QueryRow(r.ctx(), `
		SELECT f.name FROM instances i JOIN flavors f ON i.flavor_id = f.id WHERE i.id = $1
	`, r.instanceID).Scan(&flavorName); err != nil {
		r.notFound()
		return
	}
	r.ok(flavorName)
}

// ec2LocalIPv4: the request's source IP is the instance's fixed IP — the TCP peer, as in
// instanceFromRequest, never a forwarding header.
func ec2LocalIPv4(r ec2Request) { r.ok(r.c.RemoteIP()) }

// ec2AMILaunchIndex: Lightstack creates one instance per request, so the index is always 0. Constant rather
// than absent — a datasource that asks for it treats a 404 as a failed boot step.
func ec2AMILaunchIndex(r ec2Request) { r.ok("0") }

// ec2PublicIPv4 answers only when a floating IP is actually associated. Mirroring local-ipv4 here
// would tell a guest it has a routable address when it does not, so an unassociated instance gets
// 404 — the same distinction upstream makes, and why this entry is hidden from the listing.
func ec2PublicIPv4(r ec2Request) {
	var fip string
	err := r.svc.pool.QueryRow(r.ctx(), `
		SELECT f.floating_ip_address
		FROM floating_ips f
		JOIN ports p ON p.id = f.port_id
		WHERE p.device_id = $1 AND f.port_id IS NOT NULL
		LIMIT 1
	`, r.instanceID).Scan(&fip)
	if err != nil || fip == "" {
		r.notFound()
		return
	}
	r.ok(fip)
}

func ec2Placement(r ec2Request) { r.ok("availability-zone") }

// ec2AvailabilityZone: "nova" is the same default GetMetaDataJSON reports. Lightstack has no per-instance AZ
// column, so the two views agree by construction rather than by coincidence.
func ec2AvailabilityZone(r ec2Request) { r.ok(defaultAvailabilityZone) }

func ec2BlockDeviceMapping(r ec2Request) { r.ok(strings.Join([]string{"ami", "root"}, "\n")) }

// ec2BootDisk answers both ami and root. Lightstack attaches the root disk as the first virtio device and
// models no ephemeral or swap mapping, so there is nothing else to enumerate.
func ec2BootDisk(r ec2Request) { r.ok(rootBlockDevice) }

// ec2PublicKeyIndex lists the instance's keys as "<n>=<name>", EC2's index format.
func ec2PublicKeyIndex(r ec2Request) {
	keys, err := r.svc.publicKeysForInstance(r.ctx(), r.instanceID)
	if err != nil {
		log.Error().Err(err).Str("instance_id", r.instanceID).Msg("failed to read public keys")
		r.serverError()
		return
	}
	lines := make([]string, 0, len(keys))
	for i, k := range keys {
		lines = append(lines, fmt.Sprintf("%d=%s", i, k.name))
	}
	r.ok(strings.Join(lines, "\n"))
}

// ec2PublicKey serves the indexed children of public-keys/, which vary with the instance and so
// cannot be tree entries: "<n>" answers "openssh-key", "<n>/openssh-key" answers the key itself.
func ec2PublicKey(r ec2Request, sub string) {
	parts := strings.Split(sub, "/")

	// Decide what can be decided without the database first. This service is unauthenticated by
	// design — EC2 compatibility puts it on the link-local address for any guest to call — so a
	// malformed index must not reach the connection pool. Only the upper bound needs `keys`.
	idx, perr := strconv.Atoi(parts[0])
	if perr != nil || idx < 0 {
		r.notFound()
		return
	}

	keys, err := r.svc.publicKeysForInstance(r.ctx(), r.instanceID)
	if err != nil {
		log.Error().Err(err).Str("instance_id", r.instanceID).Msg("failed to read public keys")
		r.serverError()
		return
	}
	if idx >= len(keys) {
		r.notFound()
		return
	}
	switch {
	case len(parts) == 1 || parts[1] == "":
		r.ok("openssh-key")
	case parts[1] == "openssh-key":
		r.ok(keys[idx].key)
	default:
		r.notFound()
	}
}

// GetEC2MetaData serves the EC2-style metadata tree — the datasource cirros and
// most cloud images use — including the public-keys subtree that injects the SSH
// key (SPEC-009 WS-4 / T026). Dispatches on the wildcard path via ec2Tree.
func (svc *Service) GetEC2MetaData(c *gin.Context) {
	instanceID, err := svc.instanceFromRequest(c)
	if err != nil {
		writeIdentifyError(c, err, true)
		return
	}
	r := ec2Request{svc: svc, c: c, instanceID: instanceID}

	// Trim strips leading AND trailing slashes, so "placement/" arrives as "placement" — which is why
	// the tree needs no trailing-slash variants. The switch this replaced carried three such cases;
	// none of them could ever match.
	path := strings.Trim(c.Param("path"), "/")

	if path == "" {
		r.ok(ec2RootListing)
		return
	}
	// One lookup, via comma-ok. Every entry is known to have a handler because buildEC2RootListing
	// refuses a tree containing one that does not, so this cannot dispatch to a nil func.
	if entry, ok := ec2Tree[path]; ok {
		entry.serve(r)
		return
	}
	// The only paths the tree cannot name: public-keys/<n>[/openssh-key], which vary per instance.
	if sub, found := strings.CutPrefix(path, "public-keys/"); found {
		ec2PublicKey(r, sub)
		return
	}
	r.notFound()
}

// publicKeysForInstance returns the SSH key the instance was booted with, for the EC2
// public-keys tree. It is scoped to the instance's key_name — matching upstream Nova,
// which serves the boot keypair alone and omits the subtree when there is none — so a
// guest is not handed every key its owner happens to hold. It reports an error rather
// than a short list when the read fails: a truncated list boots the guest without its
// key and locks the user out, whereas an error lets the caller fail so cloud-init
// retries.
func (svc *Service) publicKeysForInstance(ctx context.Context, instanceID string) ([]struct{ name, key string }, error) {
	var userID string
	var keyName sql.NullString
	if err := svc.pool.QueryRow(ctx,
		`SELECT user_id, key_name FROM instances WHERE id = $1`, instanceID).Scan(&userID, &keyName); err != nil {
		return nil, err
	}
	// Booted without a keypair: no keys to serve (upstream omits public_keys entirely).
	if !keyName.Valid || keyName.String == "" {
		return nil, nil
	}
	rows, err := svc.pool.Query(ctx, `
		SELECT name, public_key
		FROM keypairs
		WHERE user_id = $1 AND name = $2
		ORDER BY name`, userID, keyName.String)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct{ name, key string }
	for rows.Next() {
		var n, k string
		if err := rows.Scan(&n, &k); err != nil {
			return nil, err
		}
		out = append(out, struct{ name, key string }{n, k})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListVersions returns available metadata API versions
func (svc *Service) ListVersions(c *gin.Context) {
	versions := []string{
		"2009-04-04",
		"openstack",
	}
	c.String(http.StatusOK, strings.Join(versions, "\n"))
}

// cidrToNetmask converts a CIDR string like "192.168.1.0/24" to "255.255.255.0".
func cidrToNetmask(cidr string) string {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		// Fall back to returning the prefix length on parse failure
		parts := strings.SplitN(cidr, "/", 2)
		if len(parts) == 2 {
			return parts[1]
		}
		return cidr
	}
	mask := ipNet.Mask
	return fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
}
