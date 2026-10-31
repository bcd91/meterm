package main

import "testing"

func TestCommandDetail(t *testing.T) {
	cases := []struct {
		command []string
		logArgs bool
		want    string
	}{
		{[]string{"mysql", "-pHUNTER2", "-e", "select 1"}, false, "mysql (+3 args)"},
		{[]string{"uptime"}, false, "uptime"},
		{[]string{"df -h | grep /srv"}, false, "df (+4 args)"},
		{[]string{"MYSQL_PWD=HUNTER2 mysql -e 'select 1'"}, false, "mysql (+3 args)"},
		{[]string{"TOKEN=HUNTER2"}, false, ""},
		{[]string{"TOKEN=HUNTER2", "curl", "x"}, false, "curl (+1 args)"},
		{[]string{"mysql", "-pHUNTER2"}, true, "mysql -pHUNTER2"}, // Record redacts this
	}
	for _, c := range cases {
		if got := commandDetail(c.command, c.logArgs); got != c.want {
			t.Errorf("%q: got %q, want %q", c.command, got, c.want)
		}
	}
	if got := connectDetail([]string{"-t", "sudo cat secret"}, false); got != "(+2 args)" {
		t.Errorf("connect: %q", got)
	}
	if got := connectDetail(nil, false); got != "" {
		t.Errorf("no args: %q", got)
	}
}
