package wire

import (
	"encoding/json"
	"testing"
)

// A runner a protocol behind reads the Hive's status and update messages
// with its own, older code: these never change with a protocol bump, or the
// bump strands every runner until the person rebuilds it by hand.
func TestUpdateOpIsFrozen(t *testing.T) {
	sum := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, c := range []struct {
		msg  any
		want string
	}{
		{Hello{Version: 9, Op: "update"}, `{"version":9,"op":"update"}`},
		{Hello{Version: 9, Op: "status"}, `{"version":9,"op":"status"}`},
		{Update{Bin: HiveBin{SHA256: sum, Size: 7, Machine: "x86_64"}, Version: 9}, `{"bin":{"sha256":"` + sum + `","size":7,"machine":"x86_64"},"version":9}`},
		{UpdateAsk{Need: true}, `{"need":true}`},
		{UpdateAsk{Error: "no"}, `{"error":"no"}`},
		{UpdateDone{}, `{}`},
		{UpdateDone{Error: "no"}, `{"error":"no"}`},
	} {
		raw, err := json.Marshal(c.msg)
		if err != nil || string(raw) != c.want {
			t.Errorf("%T = %s (%v), want %s", c.msg, raw, err, c.want)
		}
	}
	// A runner of another protocol answers a status read with only these.
	var st Status
	if err := json.Unmarshal([]byte(`{"name":"r","version":1,"binary":"`+sum+`","error":"protocol 2, this runner speaks 1"}`), &st); err != nil || st.Binary != sum || st.Version != 1 || st.Error == "" {
		t.Errorf("status = %+v (%v)", st, err)
	}
}
