package main

import (
	"reflect"
	"testing"
)

func TestReorderFlags(t *testing.T) {
	valued := map[string]bool{"n": true, "f": true}
	cases := []struct{ in, want []string }{
		{[]string{"Tokyo weather today", "-n", "1"}, []string{"-n", "1", "--", "Tokyo weather today"}},
		{[]string{"-n", "3", "q"}, []string{"-n", "3", "--", "q"}},
		{[]string{"a", "-f", "pw", "b", "-n=2"}, []string{"-f", "pw", "-n=2", "--", "a", "b"}},
		{[]string{"q", "--", "-n", "9"}, []string{"--", "q", "-n", "9"}},
		{[]string{"price -5 percent"}, []string{"--", "price -5 percent"}},
		{[]string{"-5", "weather"}, []string{"--", "-5", "weather"}},
	}
	for _, c := range cases {
		if got := reorderFlags(c.in, valued); !reflect.DeepEqual(got, c.want) {
			t.Errorf("reorderFlags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
