package neutron

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T102 (FR-057; research R50): an administrator's Networking API listings show every
// project's resources, as upstream's admin_or_owner reads do, and project_id/tenant_id narrow them to
// the named projects. A show by id reaches any project's resource; a name still resolves only in the
// administrator's own project. A member's listings and shows are unchanged (lock).

// projectResources is one resource of every kind the sixteen listings list, all of one project.
type projectResources struct {
	project                                           string
	network, subnet, port, group, rule, router        string
	floatingIP, portForwarding, qosPolicy, rbac       string
	trunk, addressScope, subnetPool, label, labelRule string
	groupName                                         string
	subnetCIDR, address                               string // the subnet's range and the port's fixed IP on it
}

// seedProjectResources seeds one resource of each kind in project; external is an external network
// the floating IP is allocated on.
func seedProjectResources(t *testing.T, project, external string) projectResources {
	t.Helper()
	r := projectResources{project: project}
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = uuid.NewString()
		}
		return out
	}
	v := ids(16)
	r.network, r.subnet, r.port, r.group, r.rule, r.router = v[0], v[1], v[2], v[3], v[4], v[5]
	r.floatingIP, r.portForwarding, r.qosPolicy, r.rbac = v[6], v[7], v[8], v[9]
	r.trunk, r.addressScope, r.subnetPool, r.label, r.labelRule = v[10], v[11], v[12], v[13], v[14]
	r.groupName = "spec033-admin-" + v[15][:8]
	octet := rand.IntN(250)
	address := fmt.Sprintf("10.57.%d.5", octet)
	r.subnetCIDR, r.address = fmt.Sprintf("10.57.%d.0/24", octet), address
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO networks (id, name, project_id, status) VALUES ($1, 'spec033-admin-net', $2, 'ACTIVE')`, []any{r.network, project}},
		{`INSERT INTO subnets (id, name, network_id, project_id, cidr) VALUES ($1, 'spec033-admin-subnet', $2, $3, $4)`,
			[]any{r.subnet, r.network, project, r.subnetCIDR}},
		{`INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips) VALUES ($1, 'spec033-admin-port', $2, $3, 'fa:16:3e:57:00:01', 'ACTIVE', $4::jsonb)`,
			[]any{r.port, r.network, project, `[{"subnet_id":"` + r.subnet + `","ip_address":"` + address + `"}]`}},
		{`INSERT INTO security_groups (id, name, project_id, description) VALUES ($1, $2, $3, '')`, []any{r.group, r.groupName, project}},
		{`INSERT INTO security_group_rules (id, security_group_id, direction, ethertype) VALUES ($1, $2, 'ingress', 'IPv4')`, []any{r.rule, r.group}},
		{`INSERT INTO routers (id, name, project_id) VALUES ($1, 'spec033-admin-router', $2)`, []any{r.router, project}},
		{`INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, status) VALUES ($1, $2, $3, $4, 'DOWN')`,
			[]any{r.floatingIP, project, external, fmt.Sprintf("198.18.%d.%d", rand.IntN(250), 2+rand.IntN(250))}},
		{`INSERT INTO port_forwardings (id, project_id, floatingip_id, internal_port_id, internal_ip_address, external_port, internal_port, protocol)
		  VALUES ($1, $2, $3, $4, $5, $6, 22, 'tcp')`, []any{r.portForwarding, project, r.floatingIP, r.port, address, 2000 + rand.IntN(60000)}},
		{`INSERT INTO qos_policies (id, project_id, name) VALUES ($1, $2, 'spec033-admin-qos')`, []any{r.qosPolicy, project}},
		{`INSERT INTO rbac_policies (id, project_id, object_type, object_id, target_tenant, action) VALUES ($1, $2, 'network', $3, $2, 'access_as_shared')`,
			[]any{r.rbac, project, r.network}},
		{`INSERT INTO trunks (id, name, description, project_id, port_id) VALUES ($1, 'spec033-admin-trunk', '', $2, $3)`, []any{r.trunk, project, r.port}},
		{`INSERT INTO address_scopes (id, project_id, name, ip_version) VALUES ($1, $2, 'spec033-admin-scope', 4)`, []any{r.addressScope, project}},
		{`INSERT INTO subnet_pools (id, project_id, name, prefixes) VALUES ($1, $2, 'spec033-admin-pool', $3)`,
			[]any{r.subnetPool, project, []string{fmt.Sprintf("10.%d.0.0/16", 100+rand.IntN(100))}}},
		{`INSERT INTO metering_labels (id, name, project_id) VALUES ($1, 'spec033-admin-label', $2)`, []any{r.label, project}},
		{`INSERT INTO metering_label_rules (id, metering_label_id, remote_ip_prefix) VALUES ($1, $2, '10.0.0.0/8')`, []any{r.labelRule, r.label}},
	} {
		mustExec(t, q.sql, q.args...)
	}
	t.Cleanup(func() {
		for _, q := range []struct {
			sql string
			id  string
		}{
			{"DELETE FROM metering_labels WHERE id = $1", r.label},
			{"DELETE FROM subnet_pools WHERE id = $1", r.subnetPool},
			{"DELETE FROM address_scopes WHERE id = $1", r.addressScope},
			{"DELETE FROM trunks WHERE id = $1", r.trunk},
			{"DELETE FROM rbac_policies WHERE id = $1", r.rbac},
			{"DELETE FROM qos_policies WHERE id = $1", r.qosPolicy},
			{"DELETE FROM floating_ips WHERE id = $1", r.floatingIP},
			{"DELETE FROM ports WHERE device_id = $1", r.router},
			{"DELETE FROM routers WHERE id = $1", r.router},
			{"DELETE FROM security_groups WHERE id = $1", r.group},
			{"DELETE FROM ports WHERE network_id = $1", r.network},
			{"DELETE FROM networks WHERE id = $1", r.network},
		} {
			mustExec(t, q.sql, q.id)
		}
	})
	return r
}

// listing is one Networking API collection: where it is listed and shown, its key, and which id of a
// project's resources it lists.
type listing struct {
	name  string
	list  func(r projectResources) string // path
	key   string
	show  func(r projectResources) string // "" when the collection has no show
	id    func(r projectResources) string
	idKey string // the field holding the id in a listed item; "id" unless said
}

func adminListings() []listing {
	return []listing{
		{name: "networks", list: func(projectResources) string { return "/v2.0/networks" }, key: "networks",
			show: func(r projectResources) string { return "/v2.0/networks/" + r.network }, id: func(r projectResources) string { return r.network }},
		{name: "subnets", list: func(projectResources) string { return "/v2.0/subnets" }, key: "subnets",
			show: func(r projectResources) string { return "/v2.0/subnets/" + r.subnet }, id: func(r projectResources) string { return r.subnet }},
		{name: "ports", list: func(projectResources) string { return "/v2.0/ports" }, key: "ports",
			show: func(r projectResources) string { return "/v2.0/ports/" + r.port }, id: func(r projectResources) string { return r.port }},
		{name: "security groups", list: func(projectResources) string { return "/v2.0/security-groups" }, key: "security_groups",
			show: func(r projectResources) string { return "/v2.0/security-groups/" + r.group }, id: func(r projectResources) string { return r.group }},
		{name: "security group rules", list: func(projectResources) string { return "/v2.0/security-group-rules" }, key: "security_group_rules",
			show: func(r projectResources) string { return "/v2.0/security-group-rules/" + r.rule }, id: func(r projectResources) string { return r.rule }},
		{name: "routers", list: func(projectResources) string { return "/v2.0/routers" }, key: "routers",
			show: func(r projectResources) string { return "/v2.0/routers/" + r.router }, id: func(r projectResources) string { return r.router }},
		{name: "floating IPs", list: func(projectResources) string { return "/v2.0/floatingips" }, key: "floatingips",
			show: func(r projectResources) string { return "/v2.0/floatingips/" + r.floatingIP }, id: func(r projectResources) string { return r.floatingIP }},
		{name: "port forwardings", list: func(r projectResources) string { return "/v2.0/floatingips/" + r.floatingIP + "/port_forwardings" }, key: "port_forwardings",
			show: func(r projectResources) string {
				return "/v2.0/floatingips/" + r.floatingIP + "/port_forwardings/" + r.portForwarding
			}, id: func(r projectResources) string { return r.portForwarding }},
		{name: "QoS policies", list: func(projectResources) string { return "/v2.0/qos/policies" }, key: "policies",
			show: func(r projectResources) string { return "/v2.0/qos/policies/" + r.qosPolicy }, id: func(r projectResources) string { return r.qosPolicy }},
		{name: "RBAC policies", list: func(projectResources) string { return "/v2.0/rbac-policies" }, key: "rbac_policies",
			show: func(r projectResources) string { return "/v2.0/rbac-policies/" + r.rbac }, id: func(r projectResources) string { return r.rbac }},
		{name: "trunks", list: func(projectResources) string { return "/v2.0/trunks" }, key: "trunks",
			show: func(r projectResources) string { return "/v2.0/trunks/" + r.trunk }, id: func(r projectResources) string { return r.trunk }},
		{name: "address scopes", list: func(projectResources) string { return "/v2.0/address-scopes" }, key: "address_scopes",
			show: func(r projectResources) string { return "/v2.0/address-scopes/" + r.addressScope }, id: func(r projectResources) string { return r.addressScope }},
		{name: "subnet pools", list: func(projectResources) string { return "/v2.0/subnetpools" }, key: "subnetpools",
			show: func(r projectResources) string { return "/v2.0/subnetpools/" + r.subnetPool }, id: func(r projectResources) string { return r.subnetPool }},
		{name: "network IP availabilities", list: func(projectResources) string { return "/v2.0/network-ip-availabilities" }, key: "network_ip_availabilities",
			show: func(r projectResources) string { return "/v2.0/network-ip-availabilities/" + r.network },
			id:   func(r projectResources) string { return r.network }, idKey: "network_id"},
		{name: "metering labels", list: func(projectResources) string { return "/v2.0/metering/metering-labels" }, key: "metering_labels",
			show: func(r projectResources) string { return "/v2.0/metering/metering-labels/" + r.label }, id: func(r projectResources) string { return r.label }},
		{name: "metering label rules", list: func(projectResources) string { return "/v2.0/metering/metering-label-rules" }, key: "metering_label_rules",
			show: func(projectResources) string { return "" }, id: func(r projectResources) string { return r.labelRule }},
	}
}

// listedIDs GETs path and returns the ids under key, or nil and the status on a failure.
func listedIDs(t *testing.T, r *gin.Engine, hdr map[string]string, path, key, idKey string) ([]string, int) {
	t.Helper()
	code, body := doReq(r, http.MethodGet, path, hdr)
	if code != http.StatusOK {
		return nil, code
	}
	var out map[string][]map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if idKey == "" {
		idKey = "id"
	}
	var ids []string
	for _, item := range out[key] {
		if id, ok := item[idKey].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids, code
}

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if strings.EqualFold(x, id) {
			return true
		}
	}
	return false
}

func TestAdministratorListsEveryProject(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	other := createTestProject(t, "spec033-admin-b")
	external := seedExternalNetwork(t, "spec033-admin-ext-"+uuid.NewString()[:6], seededProjectID, false, true)
	own := seedProjectResources(t, seededProjectID, external)
	theirs := seedProjectResources(t, other, external)
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	member := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	for _, l := range adminListings() {
		t.Run(l.name, func(t *testing.T) {
			sep := "?"
			if strings.Contains(l.list(theirs), "?") {
				sep = "&"
			}
			ids, code := listedIDs(t, r, admin, l.list(theirs), l.key, l.idKey)
			if code != http.StatusOK || !containsID(ids, l.id(theirs)) {
				t.Errorf("administrator, unfiltered: code=%d, %s listed: %v; want another project's %s", code, l.key, containsID(ids, l.id(theirs)), l.id(theirs))
			}
			for _, param := range []string{"project_id", "tenant_id"} {
				ids, code := listedIDs(t, r, admin, l.list(theirs)+sep+param+"="+other, l.key, l.idKey)
				if code != http.StatusOK || !containsID(ids, l.id(theirs)) {
					t.Errorf("administrator, ?%s=other: code=%d; want the other project's %s listed", param, code, l.id(theirs))
				}
				// The listing of a sub-collection names the parent's id, so only the narrowing of a
				// top-level collection can be told by an own-project resource left out.
				if !strings.Contains(l.list(theirs), "/floatingips/") && containsID(ids, l.id(own)) {
					t.Errorf("administrator, ?%s=other: lists the administrator's own %s; want only the named project's", param, l.id(own))
				}
			}
			if show := l.show(theirs); show != "" {
				if code, body := doReq(r, http.MethodGet, show, admin); code != http.StatusOK {
					t.Errorf("administrator, show of another project's: code=%d body=%s; want 200", code, body)
				}
				if code, _ := doReq(r, http.MethodGet, show, member); code != http.StatusNotFound {
					t.Errorf("member, show of another project's: code=%d; want 404 (lock)", code)
				}
			}
			ids, code = listedIDs(t, r, member, l.list(own), l.key, l.idKey)
			if code != http.StatusOK || containsID(ids, l.id(theirs)) || !containsID(ids, l.id(own)) {
				t.Errorf("member, unfiltered: code=%d; want the own %s listed and not the other project's (lock)", code, l.id(own))
			}
		})
	}

	t.Run("a group's name resolves in the administrator's own project", func(t *testing.T) {
		if code, _ := doReq(r, http.MethodGet, "/v2.0/security-groups/"+theirs.groupName, admin); code != http.StatusNotFound {
			t.Errorf("administrator, show of another project's group by name: code=%d; want 404", code)
		}
		if code, _ := doReq(r, http.MethodGet, "/v2.0/security-groups/"+own.groupName, admin); code != http.StatusOK {
			t.Errorf("administrator, show of an own group by name: code=%d; want 200", code)
		}
	})
}
