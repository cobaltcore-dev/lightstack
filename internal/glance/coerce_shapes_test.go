package glance

import (
	"encoding/json"
	"testing"
)

func TestPatchIntAcceptsBothShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want int64
		ok   bool
	}{
		{"json.Number, production's shape", json.Number("10"), 10, true},
		{"float64, the plain decoder's shape", float64(10), 10, true},
		{"a non-integral float", float64(1.5), 0, false},
		{"a string", "10", 0, false},
		{"a bool", true, 0, false},
		{"json.Number that is not an integer", json.Number("1.5"), 0, false},
	} {
		got, ok := patchInt(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: patchInt(%#v) = (%v, %v); want (%v, %v)", tc.name, tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
