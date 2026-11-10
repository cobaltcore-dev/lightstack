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
		 VALUES ('md-sub', $1, $2, '192.168.42.0/24', '192.168.42.1', $3) RETURNING id::text`,
		networkID, seededProjectID, []string{"1.1.1.1"}).Scan(&subnetID); err != nil {
		t.Fatalf("seed subnet: %v", err)
	}

	portID := uuid.NewString()
	var subnetV6 string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO subnets (name, network_id, project_id, cidr, gateway_ip, dns_nameservers)
		 VALUES ('md-sub-v6', $1, $2, '2001:db8::/64', '2001:db8::1', $3) RETURNING id::text`,
		networkID, seededProjectID, []string{"2001:4860:4860::8888"}).Scan(&subnetV6); err != nil {
		t.Fatal(err)
	}
	fixed := `[{"subnet_id":"` + subnetID + `","ip_address":"192.168.42.77"},` +
		`{"subnet_id":"` + subnetID + `","ip_address":"192.168.42.78"},` +
		`{"subnet_id":"` + subnetV6 + `","ip_address":"2001:db8::77"}]`
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
			Type       string   `json:"type"`
			NetworkID  string   `json:"network_id"`
			Link       string   `json:"link"`
			IPAddress  string   `json:"ip_address"`
			Netmask    string   `json:"netmask"`
			Gateway    string   `json:"gateway"`
			DNSServers []string `json:"dns_nameservers"`
			Routes     []struct {
				Network string `json:"network"`
				Netmask string `json:"netmask"`
				Gateway string `json:"gateway"`
			} `json:"routes"`
			Services []struct {
				Type    string `json:"type"`
				Address string `json:"address"`
			} `json:"services"`
		} `json:"networks"`
		Services []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("network_data.json is not JSON: %v (%s)", err, body)
	}

	// One dual-stack port with three addresses: one interface, three networks.
	if len(doc.Links) != 1 {
		t.Errorf("links = %d; want 1 (one link per port, not per address): %s", len(doc.Links), body)
	}
	if len(doc.Links) == 1 && doc.Links[0].EthernetMACAddress != "52:54:00:0a:0b:0c" {
		t.Errorf("link mac = %q; want the port's", doc.Links[0].EthernetMACAddress)
	}
	if len(doc.Networks) != 3 {
		t.Fatalf("networks = %d; want 3 (one per address): %s", len(doc.Networks), body)
	}
	ips := map[string]bool{}
	networkIDs := map[string]bool{}
	for _, n := range doc.Networks {
		if networkIDs[n.ID] {
			t.Errorf("duplicate network id %q", n.ID)
		}
		networkIDs[n.ID] = true
		if len(doc.Links) == 1 && n.Link != doc.Links[0].ID {
			t.Errorf("network link %q does not reference its port", n.Link)
		}
		ips[n.IPAddress] = true
		zero := "0.0.0.0"
		if n.Type == "ipv6" {
			zero = "::"
		}
		if len(n.Routes) != 1 || n.Routes[0].Network != zero || n.Routes[0].Netmask != zero || n.Routes[0].Gateway != n.Gateway {
			t.Errorf("invalid default route for %s: %+v", n.IPAddress, n.Routes)
		}
		if len(n.Services) != 1 || n.Services[0].Type != "dns" || len(n.DNSServers) != 1 || n.Services[0].Address != n.DNSServers[0] {
			t.Errorf("invalid DNS services for %s: %+v", n.IPAddress, n.Services)
		}
		if n.NetworkID != networkID {
			t.Errorf("network_id=%q; want %q", n.NetworkID, networkID)
		}
		if n.IPAddress == "2001:db8::77" {
			if n.Type != "ipv6" || n.Netmask != "ffff:ffff:ffff:ffff::" || n.Gateway != "2001:db8::1" {
				t.Errorf("invalid IPv6 metadata: %+v", n)
			}
			if len(n.DNSServers) != 1 || n.DNSServers[0] != "2001:4860:4860::8888" {
				t.Errorf("invalid IPv6 DNS: %v", n.DNSServers)
			}
			continue
		}
		if n.Type != "ipv4" {
			t.Errorf("IPv4 type=%q", n.Type)
		}
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
	if len(doc.Services) != 2 {
		t.Errorf("services must contain both DNS servers once: %+v", doc.Services)
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
	if doc.Networks == nil {
		t.Errorf("networks must be an empty array, not null: %s", body)
	}
}
