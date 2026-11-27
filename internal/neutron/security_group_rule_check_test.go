package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// SPEC-033 T169: a rule was stored as sent, and the port firewall wrote
// its remote_ip_prefix and protocol into the input iptables-restore reads, as root, on every node
// binding a port with the group. A newline in either wrote commands of the caller's choosing. A rule
// is now checked and normalised as upstream's converters and _validate_security_group_rule do
// (neutron master: neutron/extensions/securitygroup.py, neutron/db/securitygroups_db.py) before
// anything is stored (SPEC-033 FR-059).

// upstreamProtocolValues is upstream's sg_supported_protocols as its refusal prints it: None, 'any'
// and the names of neutron-lib's IP_PROTOCOL_MAP, in its order.
const upstreamProtocolValues = "(None, 'any', 'ah', 'dccp', 'egp', 'esp', 'gre', 'hopopt', 'icmp', 'igmp', " +
	"'ip', 'ipip', 'ipv6-encap', 'ipv6-frag', 'ipv6-icmp', 'icmpv6', 'ipv6-nonxt', 'ipv6-opts', 'ipv6-route', " +
	"'ospf', 'pgm', 'rsvp', 'sctp', 'tcp', 'udp', 'udplite', 'vrrp')"

func TestCreateSecurityGroupRuleRefusesWhatUpstreamRefuses(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	project := createTestProject(t, "src1-rule-refusals")
	hdr := neutronAuthHdr(project, false)
	group := sgTestGroup(t, project, "src1-refusals")

	invalidProtocol := func(p string) string {
		return "Security group rule protocol " + p + " not supported. Only protocol values " +
			upstreamProtocolValues + " and integer representations [0 to 255] are supported."
	}
	const invalidEtherType = " not supported. Allowed values are ['IPv4', 'IPv6']."
	cases := []struct {
		name, rule, neutronType, message string
	}{
		{"commands after a newline in the remote prefix",
			`"direction":"ingress","ethertype":"IPv4","remote_ip_prefix":"1.2.3.4\n-I ls-local 1 -j ACCEPT\n-A ls-local"`,
			"InvalidCIDR", "Invalid CIDR 1.2.3.4\n-I ls-local 1 -j ACCEPT\n-A ls-local given as IP prefix."},
		{"a host name", `"direction":"ingress","remote_ip_prefix":"example.com"`,
			"InvalidCIDR", "Invalid CIDR example.com given as IP prefix."},
		{"a prefix length past the family's", `"direction":"ingress","remote_ip_prefix":"10.0.0.0/33"`,
			"InvalidCIDR", "Invalid CIDR 10.0.0.0/33 given as IP prefix."},
		{"an address with a zone", `"direction":"ingress","ethertype":"IPv6","remote_ip_prefix":"fe80::1%eth0"`,
			"InvalidCIDR", "Invalid CIDR fe80::1%eth0 given as IP prefix."},
		{"a mask that is no netmask or hostmask", `"direction":"ingress","remote_ip_prefix":"10.0.0.0/255.0.255.0"`,
			"InvalidCIDR", "Invalid CIDR 10.0.0.0/255.0.255.0 given as IP prefix."},
		{"a prefix of the other family", `"direction":"ingress","ethertype":"IPv4","remote_ip_prefix":"2001:db8::/32"`,
			"SecurityGroupRuleParameterConflict", "Conflicting value ethertype IPv4 for CIDR 2001:db8::/32"},
		{"a newline in the protocol", `"direction":"ingress","protocol":"tcp\n#"`,
			"SecurityGroupRuleInvalidProtocol", invalidProtocol("tcp\n#")},
		{"a protocol number past 255", `"direction":"ingress","protocol":"256"`,
			"SecurityGroupRuleInvalidProtocol", invalidProtocol("256")},
		{"an empty protocol", `"direction":"ingress","protocol":""`,
			"SecurityGroupRuleInvalidProtocol", invalidProtocol("")},
		{"a protocol upstream does not name", `"direction":"ingress","protocol":"foo"`,
			"SecurityGroupRuleInvalidProtocol", invalidProtocol("foo")},
		{"an unknown ethertype", `"direction":"ingress","ethertype":"IPv5"`,
			"SecurityGroupRuleInvalidEtherType", "Security group rule for ethertype 'IPv5'" + invalidEtherType},
		{"an empty ethertype", `"direction":"ingress","ethertype":""`,
			"SecurityGroupRuleInvalidEtherType", "Security group rule for ethertype ''" + invalidEtherType},
		{"an unknown direction", `"direction":"sideways"`,
			"BadRequest", "Invalid input for direction. Reason: sideways is not in valid_values."},
		{"ports without a protocol", `"direction":"ingress","port_range_min":22,"port_range_max":22`,
			"SecurityGroupProtocolRequiredWithPorts", "Must also specify protocol if port range is given."},
		{"ports on a protocol without ports", `"direction":"ingress","protocol":"gre","port_range_min":1,"port_range_max":2`,
			"SecurityGroupInvalidProtocolForPort",
			"Ports cannot be specified for protocol 47. Ports are only supported for DCCP, SCTP, TCP, UDP, UDPLITE."},
		{"a range that ends before it starts", `"direction":"ingress","protocol":"tcp","port_range_min":100,"port_range_max":20`,
			"SecurityGroupInvalidPortRange", "For TCP/UDP protocols, port_range_min must be <= port_range_max"},
		{"a first port without a last", `"direction":"ingress","protocol":"tcp","port_range_min":22`,
			"SecurityGroupInvalidPortRange", "For TCP/UDP protocols, port_range_min must be <= port_range_max"},
		{"port 0", `"direction":"ingress","protocol":"udp","port_range_min":0,"port_range_max":0`,
			"SecurityGroupInvalidPortValue", "Invalid value for port 0"},
		{"a port past 65535", `"direction":"ingress","protocol":"tcp","port_range_min":22,"port_range_max":70000`,
			"SecurityGroupInvalidPortValue", "Invalid value for port 70000"},
		{"a negative port", `"direction":"ingress","protocol":"tcp","port_range_min":-1,"port_range_max":22`,
			"SecurityGroupInvalidPortValue", "Invalid value for port -1"},
		{"an ICMP type past 255", `"direction":"ingress","protocol":"icmp","port_range_min":256`,
			"SecurityGroupInvalidIcmpValue", "Invalid value for ICMP type (port_range_min) 256. It must be 0 to 255."},
		{"an ICMP code without a type", `"direction":"ingress","protocol":"icmp","port_range_max":0`,
			"SecurityGroupMissingIcmpType", "ICMP code (port-range-max) 0 is provided but ICMP type (port-range-min) is missing."},
		{"an IPv6 protocol in an IPv4 rule", `"direction":"ingress","ethertype":"IPv4","protocol":"ipv6-icmp"`,
			"SecurityGroupEthertypeConflictWithProtocol", "Invalid ethertype IPv4 for protocol ipv6-icmp."},
		{"a remote prefix and a remote group", `"direction":"ingress","remote_ip_prefix":"10.0.0.0/8","remote_group_id":"` + group + `"`,
			"SecurityGroupMultipleRemoteEntites",
			"Only one of remote_ip_prefix or remote_group_id or remote_address_group_id may be provided."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReqBody(r, http.MethodPost, "/v2.0/security-group-rules",
				`{"security_group_rule":{"security_group_id":"`+group+`",`+tc.rule+`}}`, hdr)
			if code != http.StatusBadRequest {
				t.Fatalf("code=%d body=%s; want 400", code, body)
			}
			if e := neutronError(t, body); e["type"] != tc.neutronType || e["message"] != tc.message {
				t.Errorf("refusal %v: %v; want %s: %s", e["type"], e["message"], tc.neutronType, tc.message)
			}
		})
	}

	var stored int
	if err := testPool.QueryRow(context.Background(),
		"SELECT count(*) FROM security_group_rules WHERE security_group_id = $1", group).Scan(&stored); err != nil {
		t.Fatalf("count the group's rules: %v", err)
	}
	if stored != 0 {
		t.Errorf("%d refused rule(s) stored", stored)
	}
}

func TestCreateSecurityGroupRuleNormalisesAsUpstream(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	project := createTestProject(t, "src1-rule-normalised")
	hdr := neutronAuthHdr(project, false)
	group := sgTestGroup(t, project, "src1-normalised")

	cases := []struct {
		name, rule string
		want       map[string]any // JSON's values: numbers are float64, null is nil
	}{
		{"an address is its host prefix", `"direction":"ingress","remote_ip_prefix":"192.0.2.10"`,
			map[string]any{"remote_ip_prefix": "192.0.2.10/32", "ethertype": "IPv4"}},
		{"an IPv6 address is its host prefix", `"direction":"ingress","ethertype":"IPv6","remote_ip_prefix":"2001:db8::1"`,
			map[string]any{"remote_ip_prefix": "2001:db8::1/128"}},
		{"host bits stay", `"direction":"ingress","remote_ip_prefix":"10.0.0.5/24"`,
			map[string]any{"remote_ip_prefix": "10.0.0.5/24"}},
		{"a netmask is a prefix length", `"direction":"ingress","remote_ip_prefix":"10.0.0.0/255.0.0.0"`,
			map[string]any{"remote_ip_prefix": "10.0.0.0/8"}},
		{"a hostmask is a prefix length", `"direction":"ingress","remote_ip_prefix":"10.1.0.0/0.0.255.255"`,
			map[string]any{"remote_ip_prefix": "10.1.0.0/16"}},
		{"an empty prefix is none", `"direction":"egress","remote_ip_prefix":""`,
			map[string]any{"remote_ip_prefix": nil}},
		{"a protocol name in capitals", `"direction":"ingress","protocol":"TCP","port_range_min":22,"port_range_max":22`,
			map[string]any{"protocol": "tcp", "port_range_min": 22.0, "port_range_max": 22.0}},
		{"any is none", `"direction":"ingress","protocol":"any"`,
			map[string]any{"protocol": nil}},
		{"a protocol number stays a number", `"direction":"ingress","protocol":"17","port_range_min":53,"port_range_max":53`,
			map[string]any{"protocol": "17"}},
		{"the ethertype in any case", `"direction":"ingress","ethertype":"ipv6"`,
			map[string]any{"ethertype": "IPv6"}},
		{"an IPv6 rule's ICMP by name", `"direction":"ingress","ethertype":"IPv6","protocol":"icmp"`,
			map[string]any{"protocol": "ipv6-icmp"}},
		{"an IPv6 rule's ICMP by number", `"direction":"ingress","ethertype":"IPv6","protocol":"1"`,
			map[string]any{"protocol": "58"}},
		{"every port is no port", `"direction":"ingress","protocol":"tcp","port_range_min":1,"port_range_max":65535`,
			map[string]any{"port_range_min": nil, "port_range_max": nil}},
		{"an ICMP type without a code", `"direction":"ingress","protocol":"icmp","port_range_min":8`,
			map[string]any{"port_range_min": 8.0, "port_range_max": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReqBody(r, http.MethodPost, "/v2.0/security-group-rules",
				`{"security_group_rule":{"security_group_id":"`+group+`",`+tc.rule+`}}`, hdr)
			if code != http.StatusCreated {
				t.Fatalf("create: code=%d body=%s; want 201", code, body)
			}
			created := ruleFields(t, body)
			wantRuleFields(t, "the answer", created, tc.want)

			code, body = doReq(r, http.MethodGet, "/v2.0/security-group-rules/"+created["id"].(string), hdr)
			if code != http.StatusOK {
				t.Fatalf("show: code=%d body=%s; want 200", code, body)
			}
			wantRuleFields(t, "the stored rule", ruleFields(t, body), tc.want)
		})
	}
}

func ruleFields(t *testing.T, body string) map[string]any {
	t.Helper()
	var answer struct {
		Rule map[string]any `json:"security_group_rule"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("parse %s: %v", body, err)
	}
	return answer.Rule
}

func wantRuleFields(t *testing.T, what string, got, want map[string]any) {
	t.Helper()
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s: %s = %#v; want %#v", what, key, got[key], value)
		}
	}
}
