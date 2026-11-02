package metadata

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Review 2026-10-13 N-7: GetNetworkDataJSON selected `p.fixed_ip`, a column
// that does not exist — ports store `fixed_ips` as a JSONB array — so the
// query failed and every guest fetching network_data.json got a 500, leaving
// cloud-init to fall back to DHCP or configure nothing. This pins the fixed
// shape: one link per port, one network entry per address, resolved through
// the address's own subnet (cidr, netmask, gateway, DNS).
func TestNetworkDataJSONServesPortAddresses(t *testing.T) {
	requireDB(t)
	ctx := t.Context()

	instanceID := seedInstance(t, nil)

	var networkID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO networks (name, project_id, status, admin_state_up)
		 VALUES ('md-net', $1, 'ACTIVE', true) RETURNING id::text`,
		seededProjectID).Scan(&networkID); err != nil {
		t.Fatalf("seed network: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM networks WHERE id::text = $1", networkID) })

	var subnetID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO subnets (name, network_id, project_id, cidr, gateway_ip, dns_nameservers)
		 VALUES ('md-sub', $1, $2, '192.168.42.0/24', '192.168.42.1', ARRAY['1.1.1.1']) RETURNING id::text`,
		networkID, seededProjectID).Scan(&subnetID); err != nil {
		t.Fatalf("seed subnet: %v", err)
	}

	portID := uuid.NewString()
	fixed := `[{"subnet_id":"` + subnetID + `","ip_address":"192.168.42.77"},` +
		`{"subnet_id":"` + subnetID + `","ip_address":"192.168.42.78"}]`
	if _, err := testPool.Exec(ctx,
		`INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, status, fixed_ips)
		 VALUES ($1, 'md-port', $2, $3, $4, 'compute:nova', '52:54:00:0a:0b:0c', 'ACTIVE', $5::jsonb)`,
		portID, networkID, seededProjectID, instanceID, fixed); err != nil {
		t.Fatalf("seed port: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM ports WHERE id::text = $1", portID) })

	r := newMetadataTestRouter()
	code, body := metaGet(r, "/openstack/latest/network_data.json", instanceID)
	if code != http.StatusOK {
		t.Fatalf("network_data.json: code=%d body=%s; want 200 (the N-7 defect answered 500)", code, body)
	}

	var doc struct {
		Links []struct {
			ID                 string `json:"id"`
			EthernetMACAddress string `json:"ethernet_mac_address"`
		} `json:"links"`
		Networks []struct {
			ID         string   `json:"id"`
			Link       string   `json:"link"`
			IPAddress  string   `json:"ip_address"`
			Netmask    string   `json:"netmask"`
			Gateway    string   `json:"gateway"`
			DNSServers []string `json:"dns_nameservers"`
		} `json:"networks"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("network_data.json is not JSON: %v (%s)", err, body)
	}

	// One port with two addresses: one interface, two network entries.
	if len(doc.Links) != 1 {
		t.Errorf("links = %d; want 1 (one link per port, not per address): %s", len(doc.Links), body)
	}
	if len(doc.Links) == 1 && doc.Links[0].EthernetMACAddress != "52:54:00:0a:0b:0c" {
		t.Errorf("link mac = %q; want the port's", doc.Links[0].EthernetMACAddress)
	}
	if len(doc.Networks) != 2 {
		t.Fatalf("networks = %d; want 2 (one per address): %s", len(doc.Networks), body)
	}
	ips := map[string]bool{}
	for _, n := range doc.Networks {
		ips[n.IPAddress] = true
		if n.Netmask != "255.255.255.0" {
			t.Errorf("network %s: netmask = %q; want 255.255.255.0", n.IPAddress, n.Netmask)
		}
		if n.Gateway != "192.168.42.1" {
			t.Errorf("network %s: gateway = %q; want the subnet's", n.IPAddress, n.Gateway)
		}
		if len(n.DNSServers) != 1 || n.DNSServers[0] != "1.1.1.1" {
			t.Errorf("network %s: dns = %v; want [1.1.1.1]", n.IPAddress, n.DNSServers)
		}
	}
	if !ips["192.168.42.77"] || !ips["192.168.42.78"] {
		t.Errorf("networks missing the port's addresses: %s", body)
	}
}

// TestNetworkDataJSONToleratesNullFixedIPs pins the metadata half of the
// JSON-scalar-null defect. A port on a subnet-less network persisted JSON
// `null` in fixed_ips; COALESCE does not rescue that (JSON null is not SQL
// NULL), so jsonb_array_elements raised and network_data.json answered 500.
func TestNetworkDataJSONToleratesNullFixedIPs(t *testing.T) {
	requireDB(t)
	ctx := t.Context()

	instanceID := seedInstance(t, nil)

	var networkID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO networks (name, project_id, status, admin_state_up)
		 VALUES ('md-null-net', $1, 'ACTIVE', true) RETURNING id::text`,
		seededProjectID).Scan(&networkID); err != nil {
		t.Fatalf("seed network: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM networks WHERE id::text = $1", networkID) })

	portID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, status, fixed_ips)
		 VALUES ($1, 'md-null-port', $2, $3, $4, 'compute:nova', '52:54:00:0a:0b:0d', 'ACTIVE', 'null'::jsonb)`,
		portID, networkID, seededProjectID, instanceID); err != nil {
		t.Fatalf("seed port: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM ports WHERE id::text = $1", portID) })

	code, body := metaGet(newMetadataTestRouter(), "/openstack/latest/network_data.json", instanceID)
	if code != http.StatusOK {
		t.Fatalf("network_data.json with a JSON-null fixed_ips: code=%d body=%s; want 200", code, body)
	}

	var doc struct {
		Links    []json.RawMessage `json:"links"`
		Networks []json.RawMessage `json:"networks"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("network_data.json is not JSON: %v (%s)", err, body)
	}
	if len(doc.Links) != 1 {
		t.Errorf("links = %d; want the address-less port's one link: %s", len(doc.Links), body)
	}
	if len(doc.Networks) != 0 {
		t.Errorf("networks = %d; an address-less port contributes none: %s", len(doc.Networks), body)
	}
}
