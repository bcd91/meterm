package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meterm/internal/lockfile"
	"meterm/internal/team"
)

// While another meterm holds a team's clone, team commands say it's busy
// and to retry, not that you aren't in the team, and `team list` shows the
// team as busy instead of leaving it out. Against a local bare repository.
func TestTeamCommandsSayBusy(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := cliSandbox(t)
	t.Setenv("METERM_TEAM_DIR", filepath.Join(dir, "team"))
	gitcfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(gitcfg, []byte("[user]\n\temail = alice@example.com\n\tname = Alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitcfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	remote := filepath.Join(dir, "team.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err := runRoot(t, "team", "git", "add", "ACME", remote); err != nil {
		t.Fatalf("team git add: %v", err)
	}
	defer team.SetRepoLockWait(100 * time.Millisecond)()
	st, err := team.LoadState(filepath.Join(dir, "team"))
	if err != nil || len(st.GitTeams) != 1 {
		t.Fatalf("state %+v, %v", st, err)
	}
	var g *team.GitStore
	for _, gt := range st.GitTeams {
		if g, err = team.OpenGitStore(context.Background(), filepath.Join(dir, "team"), gt); err != nil {
			t.Fatal(err)
		}
	}
	unlock, err := lockfile.Acquire(g.Dir+".lock", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, args := range [][]string{{"team", "members"}, {"team", "members", "--team", "ACME"}, {"team", "status"}, {"team", "use", "ACME"}} {
		err := runRoot(t, args...)
		if err == nil || !strings.Contains(err.Error(), "team sync busy") || !strings.Contains(err.Error(), "retry") || strings.Contains(err.Error(), "not in a team") {
			t.Errorf("%v while busy: %v; want team sync busy … retry", args, err)
		}
	}
	var out string
	var lerr error
	out = stdout(t, func() { lerr = runRoot(t, "team", "list") })
	if !strings.Contains(out, "ACME") || !strings.Contains(out, "(busy, try again)") {
		t.Errorf("team list while busy: %q; want ACME shown busy", out)
	}
	if lerr == nil || !strings.Contains(lerr.Error(), "retry") {
		t.Errorf("team list while busy: err %v; want it to say it isn't a whole answer", lerr)
	}
	unlock()
	out = stdout(t, func() { lerr = runRoot(t, "team", "list") })
	if lerr != nil || !strings.Contains(out, "owner") || strings.Contains(out, "busy") {
		t.Errorf("team list after: %q, %v", out, lerr)
	}
}
