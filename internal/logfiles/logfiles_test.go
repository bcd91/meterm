package logfiles

import "testing"

const sampleLsOutput = `total 2041052
drwxr-xr-x  2 root     adm        61440 Sep 13 06:25 .
drwxrwxr-x 11 root     syslog      4096 Sep 13 06:25 ..
-rw-r-----  1 www-data adm      3045532 Sep 13 21:45 access.log
-rw-r-----  1 www-data adm      4555686 Sep 13 06:23 access.log.1
-rw-r-----  1 www-data adm       315617 Sep  4 06:24 access.log.10.gz
-rw-r-----  1 www-data adm       958446 Sep 13 21:45 api_acetec.access.log
-rw-r-----  1 www-data adm      1488871 Sep 13 06:24 api_acetec.access.log.1
`

func TestParseDefaultHidesRotatedAndCompressed(t *testing.T) {
	entries := Parse(sampleLsOutput, false)
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	for _, want := range []string{"access.log", "api_acetec.access.log"} {
		if !names[want] {
			t.Errorf("expected current file %q to be present, got %v", want, names)
		}
	}
	for _, unwanted := range []string{"access.log.1", "access.log.10.gz", "api_acetec.access.log.1", ".", ".."} {
		if names[unwanted] {
			t.Errorf("expected %q to be filtered out by default, got %v", unwanted, names)
		}
	}
}

func TestParseAllShowsEverything(t *testing.T) {
	entries := Parse(sampleLsOutput, true)
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	for _, want := range []string{"access.log", "access.log.1", "access.log.10.gz"} {
		if !names[want] {
			t.Errorf("expected %q to be shown with all=true, got %v", want, names)
		}
	}
}

func TestParseCapturesSize(t *testing.T) {
	entries := Parse(sampleLsOutput, false)
	for _, e := range entries {
		if e.Name == "access.log" && e.Size != 3045532 {
			t.Errorf("access.log Size = %d, want 3045532", e.Size)
		}
	}
}

func TestIsRotated(t *testing.T) {
	cases := map[string]bool{
		"access.log":            false,
		"api_payment.error.log": false,
		"access.log.1":          true,
		"access.log.23":         true,
		"access.log.1.gz":       true,
		"access.log.gz":         true,
	}
	for name, want := range cases {
		if got := IsRotated(name); got != want {
			t.Errorf("IsRotated(%q) = %v, want %v", name, got, want)
		}
	}
}
