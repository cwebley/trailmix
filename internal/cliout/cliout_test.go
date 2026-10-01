package cliout

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteLeadsWithTheSchemaVersion(t *testing.T) {
	for _, c := range []struct {
		value  any
		indent bool
		want   string
	}{
		{struct {
			B string `json:"b"`
			A int    `json:"a"`
		}{"x", 2}, false, `{"schema_version":1,"b":"x","a":2}` + "\n"},
		{struct{}{}, false, `{"schema_version":1}` + "\n"},
		{map[string]int{"n": 1}, true, "{\n  \"schema_version\": 1,\n  \"n\": 1\n}\n"},
	} {
		var out bytes.Buffer
		if err := Write(&out, c.value, c.indent); err != nil || out.String() != c.want {
			t.Errorf("Write(%+v) = %q, %v; want %q", c.value, out.String(), err, c.want)
		}
	}
	var out bytes.Buffer
	if err := Write(&out, []int{1}, false); err == nil || !strings.Contains(err.Error(), "object") || out.Len() != 0 {
		t.Errorf("array accepted: %q %v", out.String(), err)
	}
}
