package common

// FixedIPs is a port's fixed_ips column — the addresses assigned to the port. It scans from the
// jsonb/json column on either backend, and both Neutron (router-interface lookup) and the
// metadata service (instance-by-address lookup) read it to find which port holds an address.
type FixedIPs []struct {
	IPAddress string `json:"ip_address"`
	SubnetID  string `json:"subnet_id"`
}

// Holds reports whether ip is one of the port's fixed addresses, compared as a whole string.
func (f FixedIPs) Holds(ip string) bool {
	for _, e := range f {
		if e.IPAddress == ip {
			return true
		}
	}
	return false
}
