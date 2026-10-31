// Package authkeys reads and prunes the authorized_keys files on a host:
// who can log in, and taking that away when someone leaves.
package authkeys

import (
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ListScript prints every authorized_keys file it can read as
// "### <owner> <path>[ self]" followed by its lines: the login user's own file, and,
// when passwordless sudo works, root's and every /home user's too. The first
// line says whether sudo was available, since without it other users' keys
// are invisible rather than absent. Read-only.
const ListScript = `me=$(id -un)
if sudo -n true </dev/null 2>/dev/null; then echo "sudo=yes"; S=1; else echo "sudo=no"; S=; fi
f="$HOME/.ssh/authorized_keys"
if [ -r "$f" ]; then echo "### $me $f self"; cat "$f" </dev/null; fi
if [ -n "$S" ]; then
  for f in /root/.ssh/authorized_keys /home/*/.ssh/authorized_keys; do
    [ "$f" = "$HOME/.ssh/authorized_keys" ] && continue
    sudo -n test -f "$f" </dev/null 2>/dev/null || continue
    u=$(sudo -n stat -c %U "$f" </dev/null 2>/dev/null || echo "?")
    echo "### $u $f"; sudo -n cat "$f" </dev/null
  done
fi
exit 0
`

// Entry is one key line in one authorized_keys file.
type Entry struct {
	Owner       string `json:"owner"` // the account the key logs in as
	File        string `json:"file"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"` // SHA256:…
	Comment     string `json:"comment,omitempty"`
	Options     string `json:"options,omitempty"` // from="…", command="…", …
	Blob        string `json:"-"`                 // base64 key, the exact text to match when revoking
	Own         bool   `json:"own"`               // the login user's file (no sudo needed to change it)
}

// Listing is what ListScript found on one host.
type Listing struct {
	Sudo    bool
	Entries []Entry
	Files   []string // every file read, including empty ones
}

// Parse reads ListScript's output. Lines that aren't keys (blank, comments,
// garbage) are skipped.
func Parse(out string) Listing {
	var l Listing
	var owner, file string
	var self bool
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "sudo="):
			l.Sudo = line == "sudo=yes"
			continue
		case strings.HasPrefix(line, "### "):
			fs := strings.Fields(strings.TrimPrefix(line, "### "))
			if len(fs) >= 2 {
				owner, file, self = fs[0], fs[1], len(fs) > 2 && fs[2] == "self"
				l.Files = append(l.Files, file)
			}
			continue
		}
		if file == "" {
			continue
		}
		if e, ok := parseLine(line); ok {
			e.Owner, e.File, e.Own = owner, file, self
			l.Entries = append(l.Entries, e)
		}
	}
	return l
}

func parseLine(line string) (Entry, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return Entry{}, false
	}
	pk, comment, options, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return Entry{}, false
	}
	blob := strings.Fields(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))))[1]
	return Entry{
		Type:        pk.Type(),
		Fingerprint: ssh.FingerprintSHA256(pk),
		Comment:     comment,
		Options:     strings.Join(options, ","),
		Blob:        blob,
	}, true
}

// PublicKey is a key to look for, read from a .pub file or an
// authorized_keys-style line.
type PublicKey struct {
	Fingerprint, Blob, Comment string
}

// ParsePublicKey reads one "ssh-ed25519 AAAA… comment" line.
func ParsePublicKey(line string) (PublicKey, error) {
	e, ok := parseLine(line)
	if !ok {
		return PublicKey{}, fmt.Errorf("not an SSH public key")
	}
	return PublicKey{Fingerprint: e.Fingerprint, Blob: e.Blob, Comment: e.Comment}, nil
}

// Matcher picks entries to revoke: by fingerprint, by exact key, or by
// comment (the usual user@machine), case-insensitive.
type Matcher struct {
	Keys     []PublicKey
	Comments []string
}

func (m Matcher) Match(e Entry) bool {
	for _, k := range m.Keys {
		if k.Fingerprint == e.Fingerprint {
			return true
		}
	}
	for _, c := range m.Comments {
		if c != "" && strings.EqualFold(c, e.Comment) {
			return true
		}
	}
	return false
}

var safePath = regexp.MustCompile(`^/[A-Za-z0-9_.@/-]+$`)

// RevokeScript removes the given key blobs from one file, after copying it
// to <file>.meterm-bak-<stamp>. It rewrites the file in place (cat >), so
// its owner and mode don't change, and prints "revoked <file>" on success.
// sudo is used for a file that isn't the login user's.
func RevokeScript(file string, blobs []string, sudo bool, stamp string) (string, error) {
	if !safePath.MatchString(file) {
		return "", fmt.Errorf("refusing to edit %q: unexpected characters in the path", file)
	}
	var pats []string
	for _, b := range blobs {
		if b == "" || strings.ContainsAny(b, "'\"\\ \n") {
			return "", fmt.Errorf("unexpected key data")
		}
		pats = append(pats, "-e '"+b+"'")
	}
	p := ""
	if sudo {
		p = "sudo -n "
	}
	return fmt.Sprintf(`f='%[1]s'
%[2]scp -p "$f" "$f.meterm-bak-%[3]s" </dev/null || { echo "backup of $f failed; nothing changed"; exit 1; }
%[2]ssh -c "grep -vF %[4]s '$f' > '$f.meterm-new'; cat '$f.meterm-new' > '$f' && rm -f '$f.meterm-new'" </dev/null || exit 1
echo "revoked $f"
`, file, p, stamp, strings.Join(pats, " ")), nil
}
