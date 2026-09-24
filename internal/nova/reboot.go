package nova

import "fmt"

// parseRebootType checks a reboot action's body the way upstream's schema does
// (nova/api/openstack/compute/schemas/servers.py): an object whose only property is
// type, one of HARD, Hard, hard, SOFT, Soft or soft. It reports whether the reboot is
// hard.
func parseRebootType(body any) (hard bool, err error) {
	obj, ok := body.(map[string]any)
	if !ok {
		return false, fmt.Errorf("reboot must be an object with a type")
	}
	for key := range obj {
		if key != "type" {
			return false, fmt.Errorf("additional properties are not allowed (%q was unexpected)", key)
		}
	}
	rebootType, ok := obj["type"]
	if !ok {
		return false, fmt.Errorf("'type' is a required property")
	}
	switch rebootType {
	case "HARD", "Hard", "hard":
		return true, nil
	case "SOFT", "Soft", "soft":
		return false, nil
	}
	return false, fmt.Errorf("type %v is not one of HARD, Hard, hard, SOFT, Soft or soft", rebootType)
}
