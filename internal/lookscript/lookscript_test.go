package lookscript

import (
	"strings"
	"testing"
)

func TestRead(t *testing.T) {
	for _, c := range []struct {
		name         string
		in           string
		reached, ran bool
		body         string
	}{
		{"posix", "meterm-look\nmeterm-sh\n[]\n", true, true, "[]\n"},
		{"startup text first", "Welcome\nmotd no newline meterm-look\nmeterm-sh\nx", true, true, "x"},
		{"cmd, no sh", "meterm-look \r\n", true, false, ""},
		{"crlf", "meterm-look \r\nmeterm-sh\r\nx\r\n", true, true, "x\r\n"},
		{"sh marker last", "meterm-look\nmeterm-sh", true, true, ""},
		{"nothing", "", false, false, ""},
		{"marker inside a line isn't one", "say meterm-sh now\n", false, false, ""},
		{"only the first sh marker counts", "meterm-look\nmeterm-sh\nmeterm-sh\n", true, true, "meterm-sh\n"},
	} {
		got := Read([]byte(c.in))
		if got.Reached != c.reached || got.Ran != c.ran || string(got.Body) != c.body || (got.Ran && got.Body == nil) || (!got.Ran && got.Body != nil) {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

// Start is one line that csh and fish read as sh does: no newline, no !, and the only
// single quote is the one that opens sh's script.
func TestStart(t *testing.T) {
	if strings.ContainsAny(Start, "\n\r!") || strings.Count(Start, "'") != 1 || !strings.HasSuffix(Start, "; ") {
		t.Errorf("%q", Start)
	}
}
