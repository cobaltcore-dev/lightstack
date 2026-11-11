package glance

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
)

// The Glance metadefs schema routes must answer 200 with a schema object. Horizon's
// Create Image modal loads /v2/schemas/metadefs/namespace and reads `.data` off the
// response; a 404 there makes the modal fail ("Unable to retrieve namespace"). Guard
// each registered path against regressing to a 404, through RegisterRoutes mounted
// as main.go mounts it.
func TestMetadefsSchemaRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	(&Service{}).RegisterRoutes(r.Group("/v2"))

	for _, p := range []string{
		"namespace", "namespaces", "resource_type", "resource_types", "object",
		"objects", "property", "properties", "tag", "tags",
	} {
		t.Run(p, func(t *testing.T) {
			body := getSchema(t, r, "/v2/schemas/metadefs/"+p)
			if _, ok := body["properties"]; !ok {
				t.Errorf("schema missing 'properties': %v", body)
			}
		})
	}
}

// Each plural metadefs schema path answers upstream Glance's collection schema
// (glance/api/v2/schemas.py returns *_collection_schema.raw()), not the singular
// document: the singular schema as the items of a property named after the
// collection, plus the paging fields and their links. Upstream keys a namespace's
// properties by name (DictCollectionSchema), so that collection is an object.
// Driven through RegisterRoutes, mounted as main.go mounts it.
func TestMetadefsCollectionSchemas(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	(&Service{}).RegisterRoutes(r.Group("/v2"))

	for _, tc := range []struct {
		plural, singular, collection, kind, key string
	}{
		{"namespaces", "namespace", "namespaces", "array", "items"},
		{"resource_types", "resource_type", "resource_type_associations", "array", "items"},
		{"objects", "object", "objects", "array", "items"},
		{"properties", "property", "properties", "object", "additionalProperties"},
		{"tags", "tag", "tags", "array", "items"},
	} {
		t.Run(tc.plural, func(t *testing.T) {
			body := getSchema(t, r, "/v2/schemas/metadefs/"+tc.plural)
			if body["name"] != tc.collection {
				t.Errorf("name = %v, want %q", body["name"], tc.collection)
			}
			props, _ := body["properties"].(map[string]any)
			members, _ := props[tc.collection].(map[string]any)
			if members["type"] != tc.kind {
				t.Errorf("properties.%s.type = %v, want %q", tc.collection, members["type"], tc.kind)
			}
			if item := getSchema(t, r, "/v2/schemas/metadefs/"+tc.singular); !reflect.DeepEqual(members[tc.key], item) {
				t.Errorf("properties.%s.%s = %v, want the %s schema %v", tc.collection, tc.key, members[tc.key], tc.singular, item)
			}
			for _, field := range []string{"first", "next", "schema"} {
				if _, ok := props[field]; !ok {
					t.Errorf("properties missing %q", field)
				}
			}
			var rels []string
			links, _ := body["links"].([]any)
			for _, l := range links {
				link, _ := l.(map[string]any)
				rel, _ := link["rel"].(string)
				rels = append(rels, rel)
			}
			if want := []string{"first", "next", "describedby"}; !slices.Equal(rels, want) {
				t.Errorf("link rels = %v, want %v", rels, want)
			}
		})
	}
}

// getSchema GETs a schema document and decodes it, failing the test on anything
// but a 200 with a JSON object.
func getSchema(t *testing.T, r *gin.Engine, path string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: body not a JSON object: %v", path, err)
	}
	return body
}
