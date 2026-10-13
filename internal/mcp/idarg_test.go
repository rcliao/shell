package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIDArg(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr string
	}{
		{`{"id":"1553811996682489860"}`, 1553811996682489860, ""}, // 19-digit snowflake, exact as a string
		{`{"id":"-100200300"}`, -100200300, ""},
		{`{"id":42}`, 42, ""},
		{`{"id":-100200300}`, -100200300, ""},
		{`{"id":""}`, 0, ""},
		{`{"id":null}`, 0, ""},
		{`{}`, 0, ""},
		{`{"id":9007199254740992}`, 9007199254740992, ""},
		// What a JS client sends after rounding …489860: refuse, don't guess.
		{`{"id":1553811996682489900}`, 0, "pass it as a string"},
		{`{"id":-9007199254740993}`, 0, "pass it as a string"},
		{`{"id":1.5538119966824899e+18}`, 0, "pass it as a string"},
		{`{"id":"12ab"}`, 0, "not an integer"},
	}
	for _, c := range cases {
		var p struct {
			ID idArg `json:"id"`
		}
		err := json.Unmarshal([]byte(c.in), &p)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err = %v, want containing %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected err %v", c.in, err)
			continue
		}
		if int64(p.ID) != c.want {
			t.Errorf("%s: got %d, want %d", c.in, p.ID, c.want)
		}
	}
}

// The id travels on to the RPC server as a JSON number, decoded there into
// int64 — Go to Go, so every digit survives.
func TestIDArgForwardsExactly(t *testing.T) {
	b, _ := json.Marshal(map[string]any{"message_thread_id": idArg(1553811996682489860)})
	var got struct {
		ID int64 `json:"message_thread_id"`
	}
	if err := json.Unmarshal(b, &got); err != nil || got.ID != 1553811996682489860 {
		t.Fatalf("forwarded %s → %d, %v", b, got.ID, err)
	}
}
