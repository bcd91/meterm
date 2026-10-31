package activity

import (
	"os"
	"strings"
	"testing"
)

func TestRedactMasksCredentials(t *testing.T) {
	for _, in := range []string{
		"mysql -pHUNTER2 -e select 1",
		"/usr/bin/mysqldump -u root -pHUNTER2 db",
		"sudo mariadb -p'HUNTER2' db",
		`sh -c 'mysql -pHUNTER2 db'`,
		"sshpass -p HUNTER2 ssh web1",
		"sshpass -pHUNTER2 ssh web1",
		"app --password=HUNTER2 start",
		"app --password HUNTER2 start",
		"app --db-password HUNTER2",
		"app --token HUNTER2",
		"app --api-key=HUNTER2",
		"app --client-secret HUNTER2",
		"PGPASSWORD=HUNTER2 psql db",
		"export API_KEY=HUNTER2",
		"AWS_SECRET_ACCESS_KEY=HUNTER2 aws s3 ls",
		`curl -H "Authorization: Bearer HUNTER2" https://example.com`,
		"curl -H Authorization:HUNTER2 https://example.com",
		"curl -H X-Api-Key:HUNTER2 https://example.com",
		"curl -u admin:HUNTER2 https://example.com",
		"curl --user admin:HUNTER2 https://example.com",
		"git clone https://admin:HUNTER2@example.com/repo.git",
		"curl https://example.com/x?a=1&token=HUNTER2&b=2",
		`echo {"password":"HUNTER2"}`,
		"grep password: HUNTER2",
		"echo -----BEGIN OPENSSH PRIVATE KEY-----\nHUNTER2\n-----END OPENSSH PRIVATE KEY-----",
	} {
		got := Redact(in)
		if strings.Contains(got, "HUNTER2") {
			t.Errorf("Redact(%q) = %q, secret still there", in, got)
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("Redact(%q) = %q, no marker", in, got)
		}
	}
	for _, in := range []string{
		"ghp_abcdefghijklmnopqrstuvwxyz012345",
		"github_pat_abcdefghijklmnopqrstuvwxyz",
		"xoxb-1234567890-abcdefghij",
		"AKIAABCDEFGHIJKLMNOP",
		"sk-abcdefghijklmnopqrstuvwxyz",
		"eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.c2lnbmF0dXJlMTIz",
	} {
		if got := Redact("deploy " + in); got != "deploy "+redacted {
			t.Errorf("Redact(%q) = %q", in, got)
		}
	}
}

// What's left has to stay readable: this is an audit log.
func TestRedactKeepsOrdinaryCommands(t *testing.T) {
	for _, in := range []string{
		"",
		"uptime",
		"systemctl restart nginx",
		"mkdir -p /srv/app",
		"mkdir -p/srv/app",
		"ssh -p2222 web1",
		"mysql -u root -p db",
		"mysql -P3306 -h db1 app",
		"cat /etc/passwd",
		"cat secrets.txt",
		"rm token.json",
		"docker login --password-stdin ghcr.io",
		"app --password-file /run/pw start",
		"app --password-file=/run/pw start",
		"psql --no-password db",
		"app --token --verbose",
		"docker run -u 1000:1000 nginx",
		"docker run -p 80:80 nginx",
		"ssh://git@github.com/x.git",
		"curl -o out https://example.com/a",
		"redis-cli -h db1 ping",
		"ssh-keygen -t ed25519 -f k",
		"https://example.com/a?b=1",
		"upload /a/b.txt -> /c/d.txt",
		"dev api",
		"-L 8080:localhost:80",
	} {
		if got := Redact(in); got != in {
			t.Errorf("Redact(%q) = %q, want it unchanged", in, got)
		}
	}
}

// The forms a review found getting through, each with what must not survive.
func TestRedactCatchesTheseForms(t *testing.T) {
	for _, c := range []struct{ in, secret string }{
		{`curl -d "{\"password\":\"hunter2\"}" https://x`, "hunter2"},
		{`curl -su bob:hunter2 https://x`, "hunter2"},
		{`curl -ubob:hunter2 https://x`, "hunter2"},
		{`curl -H "Cookie: session=abc123secret" https://x`, "abc123secret"},
		{`curl -H Cookie: session=abc123secret; other=second https://x`, "abc123secret"},
		{`curl -H Cookie: session=abc123secret; other=second https://x`, "second"},
		{`curl -b session=abc123secret https://x`, "abc123secret"},
		{`docker login -u bob -p hunter2 reg`, "hunter2"},
		{`redis-cli -a hunter2 ping`, "hunter2"},
		{`MYSQL_PWD=hunter2 mysql -e 'select 1'`, "hunter2"},
		{`REDISCLI_AUTH=hunter2 redis-cli`, "hunter2"},
		{`export X=$(mysql -phunter2 -e 'select 1')`, "hunter2"},
		{`cd /a;mysql -phunter2`, "hunter2"},
		{`mysqlcheck -uroot -phunter2`, "hunter2"},
		{`aws configure set aws_secret_access_key AbCd1234`, "AbCd1234"},
		{`tailscale up --authkey tskey-abc123`, "abc123"},
		{`ssh-keygen -N hunter2 -f k`, "hunter2"},
		{`echo 'root:hunter2' | chpasswd`, "hunter2"},
		{`psql postgres://u:ab/cd@h/db`, "ab/cd"},
		{`psql postgres://u:ab/cd@h/db`, "cd@"},
		{`git clone https://abc123def456@git.example.com/r.git`, "abc123def456"},
		{"-----BEGIN PGP PRIVATE KEY BLOCK-----\nhunter2\n-----END PGP PRIVATE KEY BLOCK-----", "hunter2"},
		{`deploy sk_live_abcdefgh1234`, "sk_live_abcdefgh1234"},
		{`curl https://hooks.slack.com/services/T000/B000/hunter2`, "hunter2"},
		{`curl https://acct.blob.example.net/c/f?sv=2024&sig=hunter2`, "hunter2"},
		{`mysql --password=ab,cd`, "cd"},
		{`mysql --password=ab,cd`, "ab"},
		{`mysql --password="a\"b c"`, "b c"},
		{`psql postgres://u:p@ss@h/db`, "ss"},
		{`echo hunter2 | sudo -S reboot`, "hunter2"},
		{`echo hunter2 | docker login --password-stdin reg`, "hunter2"},
		{`htpasswd -b /etc/pw bob hunter2`, "hunter2"},
	} {
		got := Redact(c.in)
		if strings.Contains(got, c.secret) {
			t.Errorf("Redact(%q) = %q, %q still there", c.in, got, c.secret)
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("Redact(%q) = %q, no marker", c.in, got)
		}
	}
	if got, want := Redact(`curl -H "Authorization: Bearer hunter2" https://x`), `curl -H "Authorization: `+redacted+`" https://x`; got != want {
		t.Errorf("got %q, want %q, closing quote included", got, want)
	}
}

func TestAppendRedactsBeforeHashing(t *testing.T) {
	l := newLog(t)
	e, err := l.Append(Event{Action: "exec", Host: "web1", Detail: "mysql -pHUNTER2 -e select"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.Detail, "HUNTER2") {
		t.Errorf("returned event still has the secret: %q", e.Detail)
	}
	raw, err := os.ReadFile(l.eventsPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "HUNTER2") {
		t.Errorf("the secret reached the file: %s", raw)
	}
	events, err := l.All()
	if err != nil {
		t.Fatal(err)
	}
	if broken := Verify(events); broken != nil {
		t.Errorf("chain broken after redacting: %+v", broken)
	}
}
