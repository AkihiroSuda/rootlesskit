package msgutil

import (
	"encoding/hex"
	"testing"
)

func TestMarshal(t *testing.T) {
	emptyStruct := struct{}{}
	fooStruct := struct{ Foo string }{Foo: "hello"}
	testCases := []struct {
		x interface{}
	}{
		{
			x: nil,
		},
		{
			x: 42,
		},
		{
			x: &emptyStruct,
		},
		{
			x: &fooStruct,
		},
	}
	for i, tc := range testCases {
		b, err := Marshal(tc.x)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%d: marshal %+v\n%s", i, tc.x, hex.Dump(b))
	}
}
