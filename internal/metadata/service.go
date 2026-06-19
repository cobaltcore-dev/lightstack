package metadata

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// Service handles EC2-compatible metadata service endpoints
type Service struct {
	// In production, this would run on 169.254.169.254
	// For testing, it runs on localhost:8775
	bindAddr string
}

// NewService creates a new metadata service
func NewService(bindAddr string) *Service {
	return &Service{
		bindAddr: bindAddr,
	}
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

// instanceFromIP looks up instance by source IP address
// In a real deployment, this would check which network namespace the request came from
// For testing, we'll use a header: X-Instance-ID
func (svc *Service) instanceFromRequest(c *gin.Context) (string, error) {
	// Check for test header first (only trust this in non-production contexts)
	if instanceID := c.GetHeader("X-Instance-ID"); instanceID != "" {
		return instanceID, nil
	}

	// In production, the metadata service is reachable only via the link-local
	// address 169.254.169.254 from within a network namespace. Use the source IP
	// to look up which port (and therefore instance) is making the request.
	clientIP := c.ClientIP()
	if net.ParseIP(clientIP) != nil {
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
		var instanceID string
		err := database.DB.QueryRow(c.Request.Context(),
			`SELECT device_id FROM ports
			 WHERE device_id IS NOT NULL AND fixed_ips @> $1::jsonb
			 LIMIT 1`,
			fmt.Sprintf(`[{"ip_address":%q}]`, clientIP),
		).Scan(&instanceID)
		if err == nil && instanceID != "" {
			return instanceID, nil
		}
	}

	return "", fmt.Errorf("cannot determine instance ID from request")
}

// GetMetaDataJSON returns instance metadata in JSON format (OpenStack style)
func (svc *Service) GetMetaDataJSON(c *gin.Context) {
	instanceID, err := svc.instanceFromRequest(c)
	if err != nil {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}

	var name, hostname, projectID, userID string
	var uuid string
	err = database.DB.QueryRow(c.Request.Context(), `
		SELECT id, name, name, project_id, user_id
		FROM instances
		WHERE id = $1
	`, instanceID).Scan(&uuid, &name, &hostname, &projectID, &userID)

	if errors.Is(err, pgx.ErrNoRows) {
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
		"availability_zone": "nova", // Default AZ
		"launch_index":      0,
		"meta":              gin.H{}, // Custom metadata key-value pairs
		"public_keys":       gin.H{}, // SSH keys
	}

	// Fetch custom metadata
	rows, err := database.DB.Query(c.Request.Context(), `
		SELECT key, value
		FROM instance_metadata
		WHERE instance_id = $1
	`, instanceID)
	if err == nil {
		defer rows.Close()
		metaMap := make(map[string]string)
		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err == nil {
				metaMap[key] = value
			}
		}
		if err := rows.Err(); err != nil {
			log.Error().Err(err).Msg("rows iteration error")
		}
		if len(metaMap) > 0 {
			metadata["meta"] = metaMap
		}
	}

	// Fetch SSH keys
	keyRows, err := database.DB.Query(c.Request.Context(), `
		SELECT k.name, k.public_key
		FROM keypairs k
		WHERE k.user_id = $1
	`, userID)
	if err == nil {
		defer keyRows.Close()
		keysMap := make(map[string]string)
		for keyRows.Next() {
			var name, pubkey string
			if err := keyRows.Scan(&name, &pubkey); err == nil {
				keysMap[name] = pubkey
			}
		}
		if len(keysMap) > 0 {
			metadata["public_keys"] = keysMap
		}
	}

	c.JSON(http.StatusOK, metadata)
}

// GetUserData returns cloud-init user-data
func (svc *Service) GetUserData(c *gin.Context) {
	instanceID, err := svc.instanceFromRequest(c)
	if err != nil {
		c.String(http.StatusNotFound, "")
		return
	}

	var userData string
	err = database.DB.QueryRow(c.Request.Context(), `
		SELECT userdata
		FROM instance_userdata
		WHERE instance_id = $1
	`, instanceID).Scan(&userData)

	if errors.Is(err, pgx.ErrNoRows) {
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
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}

	// Query all ports for this instance
	rows, err := database.DB.Query(c.Request.Context(), `
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
			continue
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
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
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

// GetEC2MetaData serves the EC2-style metadata tree — the datasource cirros and
// most cloud images use — including the public-keys subtree that injects the SSH
// key (SPEC-009 WS-4 / T026). Dispatches on the wildcard path.
func (svc *Service) GetEC2MetaData(c *gin.Context) {
	instanceID, err := svc.instanceFromRequest(c)
	if err != nil {
		c.String(http.StatusNotFound, "")
		return
	}
	ctx := c.Request.Context()
	path := strings.Trim(c.Param("path"), "/")

	switch {
	case path == "":
		c.String(http.StatusOK, strings.Join([]string{
			"hostname", "instance-id", "instance-type", "local-ipv4", "public-keys/",
		}, "\n"))
	case path == "instance-id":
		c.String(http.StatusOK, instanceID)
	case path == "hostname":
		var hostname string
		if err := database.DB.QueryRow(ctx, `SELECT name FROM instances WHERE id = $1`, instanceID).Scan(&hostname); err == nil {
			c.String(http.StatusOK, hostname)
		} else {
			c.String(http.StatusNotFound, "")
		}
	case path == "instance-type":
		var flavorName string
		if err := database.DB.QueryRow(ctx, `
			SELECT f.name FROM instances i JOIN flavors f ON i.flavor_id = f.id WHERE i.id = $1
		`, instanceID).Scan(&flavorName); err == nil {
			c.String(http.StatusOK, flavorName)
		} else {
			c.String(http.StatusNotFound, "")
		}
	case path == "local-ipv4":
		// The request's source IP is the instance's fixed IP.
		c.String(http.StatusOK, c.ClientIP())
	case path == "public-keys" || path == "public-keys/":
		keys := svc.publicKeysForInstance(ctx, instanceID)
		lines := make([]string, 0, len(keys))
		for i, k := range keys {
			lines = append(lines, fmt.Sprintf("%d=%s", i, k.name))
		}
		c.String(http.StatusOK, strings.Join(lines, "\n"))
	case strings.HasPrefix(path, "public-keys/"):
		// public-keys/<index> -> "openssh-key"; public-keys/<index>/openssh-key -> the key
		parts := strings.Split(strings.TrimPrefix(path, "public-keys/"), "/")
		idx, perr := strconv.Atoi(parts[0])
		keys := svc.publicKeysForInstance(ctx, instanceID)
		if perr != nil || idx < 0 || idx >= len(keys) {
			c.String(http.StatusNotFound, "")
			return
		}
		switch {
		case len(parts) == 1 || parts[1] == "":
			c.String(http.StatusOK, "openssh-key")
		case parts[1] == "openssh-key":
			c.String(http.StatusOK, keys[idx].key)
		default:
			c.String(http.StatusNotFound, "")
		}
	default:
		c.String(http.StatusNotFound, "")
	}
}

// publicKeysForInstance returns the instance owner's SSH keypairs, ordered by
// name, for the EC2 public-keys tree.
func (svc *Service) publicKeysForInstance(ctx context.Context, instanceID string) []struct{ name, key string } {
	var userID string
	if err := database.DB.QueryRow(ctx, `SELECT user_id FROM instances WHERE id = $1`, instanceID).Scan(&userID); err != nil {
		return nil
	}
	rows, err := database.DB.Query(ctx, `SELECT name, public_key FROM keypairs WHERE user_id = $1 ORDER BY name`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []struct{ name, key string }
	for rows.Next() {
		var n, k string
		if err := rows.Scan(&n, &k); err == nil {
			out = append(out, struct{ name, key string }{n, k})
		}
	}
	return out
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
