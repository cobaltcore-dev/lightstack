package neutron_test

import (
	"os"
	"testing"

	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contract "github.com/cobaltcore-dev/o3k/test/contract"
)

func skipIfO3KNotRunning(t *testing.T) {
	t.Helper()
	if os.Getenv("SKIP_CONTRACT_TESTS") == "1" {
		t.Skip("Skipping contract test (O3K not running)")
	}
}

func TestNeutronListExtensions_Contract(t *testing.T) {
	skipIfO3KNotRunning(t)

	client := contract.SetupNeutronClient(t)

	// List API extensions
	allPages, err := extensions.List(client).AllPages()
	require.NoError(t, err)

	extensionsList, err := extensions.ExtractExtensions(allPages)
	require.NoError(t, err)

	// Should have at least some standard extensions
	assert.NotEmpty(t, extensionsList)

	// Verify extension structure
	foundExtensions := make(map[string]bool)
	for _, ext := range extensionsList {
		assert.NotEmpty(t, ext.Alias)
		assert.NotEmpty(t, ext.Name)
		foundExtensions[ext.Alias] = true
	}

	// Check for common extensions
	commonExtensions := []string{"security-group", "router", "port-security"}
	for _, extAlias := range commonExtensions {
		if foundExtensions[extAlias] {
			t.Logf("Found expected extension: %s", extAlias)
		}
	}
}
