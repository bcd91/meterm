package config

import (
	"reflect"
	"testing"
)

func TestParseLocalForward(t *testing.T) {
	f, err := ParseLocalForward("5432:db.internal:5432")
	if err != nil || f != (Forward{Type: "L", LocalPort: 5432, RemoteHost: "db.internal", RemotePort: 5432}) {
		t.Fatalf("got %+v, %v", f, err)
	}
	for _, bad := range []string{"", "5432", "5432:db", "x:db:5432", "5432::5432", "0:db:1", "1:db:70000"} {
		if _, err := ParseLocalForward(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSplitTags(t *testing.T) {
	if got := SplitTags(" prod, web ,,db "); !reflect.DeepEqual(got, []string{"prod", "web", "db"}) {
		t.Errorf("got %q", got)
	}
	if got := SplitTags("  "); got != nil {
		t.Errorf("blank = %q", got)
	}
}

func TestAddForwardSkipsDuplicates(t *testing.T) {
	var h Host
	f := Forward{Type: "L", LocalPort: 1, RemoteHost: "x", RemotePort: 2}
	h.AddForward(f)
	h.AddForward(f)
	if len(h.Forwards) != 1 {
		t.Errorf("forwards = %+v", h.Forwards)
	}
}
