package lung

import (
	"reflect"
	"testing"
)

func TestCopyAndInsensitiviseMap(t *testing.T) {
	var (
		given = map[string]interface{}{
			"Foo": 32,
			"Bar": map[interface{}]interface {
			}{
				"ABc": "A",
				"cDE": "B"},
		}
		expected = map[string]interface{}{
			"foo": 32,
			"bar": map[string]interface {
			}{
				"abc": "A",
				"cde": "B"},
		}
	)

	got := copyAndInsensitiviseMap(given)

	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("Got %q\nexpected\n%q", got, expected)
	}

	if _, ok := given["foo"]; ok {
		t.Fatal("Input map changed")
	}

	if _, ok := given["bar"]; ok {
		t.Fatal("Input map changed")
	}

	m := given["Bar"].(map[interface{}]interface{})
	if _, ok := m["ABc"]; !ok {
		t.Fatal("Input map changed")
	}
}

