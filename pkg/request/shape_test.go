package request

import "testing"

func TestShapeOf(t *testing.T) {
	for _, c := range []struct {
		doc           string
		values, items int
	}{
		{`{}`, 1, 0},
		{`null`, 0, 0},
		{`{"events":[]}`, 1, 0},
		{`{"events":[{},{"signing_chain":[{},{}]}],"file_access_events":[{}]}`, 11, 3},
		{`{"events":[null, 1, -2.5e3, true, false, "x", ["y"]]}`, 9, 7},
		{`{"padding":{"x":[{},{}]}}`, 6, 0},
		{`{"events":[{"process_chain":[null,null,null]}]}`, 6, 1},
		{`{"events":[{"file_path":"{[\"}]"}]}`, 3, 1},
		{`{"machine_id":"a","events":[{}],"x":"[{"}`, 3, 1},
		{`[[{}]]`, 3, 0},
	} {
		got := ShapeOf([]byte(c.doc))
		if got.Values != c.values || got.TopLevelItems != c.items {
			t.Errorf("ShapeOf(%s) = %+v, want %d values and %d top-level items", c.doc, got, c.values, c.items)
		}
	}
}
