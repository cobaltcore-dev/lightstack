package nova_test

import (
	"testing"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
)

// testImageID is the image a test boots from: a real one, because server create refuses
// an image that does not exist (SPEC-020 FR-026).
func testImageID(t *testing.T) string {
	t.Helper()
	return contract.CreateTestImage(t)
}
