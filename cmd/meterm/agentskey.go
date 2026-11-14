package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"meterm/internal/agents"
	"meterm/internal/config"
	"meterm/internal/sshkey"
	"meterm/internal/termsafe"
)

func newAgentsKeyCmd() *cobra.Command {
	var identity string
	var check bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "key <host> [--identity file] [--check]",
		Short: "Print the authorized_keys line that limits a host's watcher key to `claude agents --json`",
		Long: "Prints one line for the host's authorized_keys: the public half of its\n" +
			"agents_identity (or --identity), with a forced command that runs only\n" +
			"`claude agents --json` and `restrict`, so the key can't open a shell, a pty or a\n" +
			"forward. It doesn't install anything or contact the host: put the line there\n" +
			"yourself, ideally in an AuthorizedKeysFile the account can't write.\n\n" +
			"It refuses a key that an ordinary login to the host could use too: the host's\n" +
			"identity_file, any IdentityFile ssh would try for it (ssh's own defaults\n" +
			"included), or a key ssh-agent holds. sshd uses the first line a key matches, so\n" +
			"such a key would either stay unlimited, or limit your own logins.\n\n" +
			"--check, after the line is in, logs in with the watcher key alone and asks for\n" +
			"something else (echo of a random word). If that comes back, the key isn't limited:\n" +
			"exit 2. If the host ran its forced command instead, it is: exit 0. Anything else,\n" +
			"such as the host refusing the key or not answering, is exit 1.\n\n" +
			"The limit is on the host's side because nothing on this machine can be: anything\n" +
			"running as you here, an agent included, can read the key and use it as it likes.\n\n" +
			"To set one up:\n" +
			"  meterm keygen --file ~/.ssh/meterm_agents\n" +
			"  # in hosts.yaml, on the host, its group or defaults: agents_identity: ~/.ssh/meterm_agents\n" +
			"  meterm agents key devbox            # then add the line on devbox\n" +
			"  meterm agents key devbox --check    # then see that it holds\n\n" +
			"A host named \"key\" is listed with `meterm agents -- key`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveCfgPath()
			if err != nil {
				return err
			}
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			if err := checkAgentsClient(agentsGOOS); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			if check {
				if identity != "" {
					return fmt.Errorf("--check uses the host's agents_identity, as a look does; --identity doesn't go with it")
				}
				jobs, err := selectAgentHosts(cfg, args, nil, nil)
				if err != nil {
					return err
				}
				msg, code := checkWatcherKey(ctx, jobs[0], timeout)
				fmt.Fprintln(os.Stderr, "meterm: "+msg)
				if code != 0 {
					finish(code)
				}
				return nil
			}
			h, idx, err := cfg.Target(args[0])
			if err != nil {
				return err
			}
			if idx < 0 {
				return fmt.Errorf("no such host: %s", exactName(args[0]))
			}
			r := cfg.Resolve(*h)
			sctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			line, account, err := watcherKeyLine(sctx, r, identity, timeout)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Add this line to the authorized_keys of %s on %s; meterm doesn't install it. Then run `meterm agents key %s --check`.\n", lbl(account), lbl(r.Name), exactName(args[0]))
			_, err = fmt.Fprintln(os.Stdout, line)
			return err
		},
	}
	cmd.Flags().StringVar(&identity, "identity", "", "the watcher key (private or .pub), instead of the host's agents_identity")
	cmd.Flags().BoolVar(&check, "check", false, "log in with the watcher key and see whether the host limits it (exit 0 yes, 2 no, 1 can't tell)")
	cmd.Flags().DurationVar(&timeout, "timeout", agentsDefaultTimeout, "give up on the host after this long")
	return cmd
}

// Exit codes of `agents key --check`.
const (
	keyCheckLimited   = 0
	keyCheckUnsure    = 1
	keyCheckUnlimited = 2
)

// checkWatcherKey logs in with the watcher key alone, as a look does, and asks for an
// echo of a random word in place of the look's command. A key the host limits runs
// its forced command instead, and the word doesn't come back. Like a look, it isn't in
// the activity log: it runs nothing but that echo.
func checkWatcherKey(ctx context.Context, job agentJob, timeout time.Duration) (string, int) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, refused := prepareLook(ctx, job, timeout)
	if refused == "" {
		refused = checkAgentsIdentity(r)
	}
	if refused != "" {
		return refused, keyCheckUnsure
	}
	if r.AgentsIdentity == "" {
		return "it has no agents_identity, so there's no watcher key to check", keyCheckUnsure
	}
	wr, sshConfig, remove, err := watcherLook(ctx, r, timeout)
	if err != nil {
		return "not checked: " + strings.TrimPrefix(err.Error(), errWatcherSettings.Error()+": "), keyCheckUnsure
	}
	defer remove()
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "not checked: no random word", keyCheckUnsure
	}
	word := "meterm-watcher-check-" + hex.EncodeToString(nonce[:])
	stdout, stderr, overflow, err := sshLookQuery(sshConfig, timeout, "echo "+word, 64<<10)(ctx, wr.Resolved)
	var exit *exec.ExitError
	exited := errors.As(err, &exit)
	switch {
	case bytes.Contains(stdout, []byte(word)):
		return "NOT LIMITED: the watcher key ran a command of meterm's choosing on " + lbl(r.Name) +
			". Anything that can read the key can run anything there. Put the line from `meterm agents key` " +
			"before any other line with this key, or take the other line out, and check again.", keyCheckUnlimited
	case wr.refused(stderr):
		return "the host refused the watcher key: the line isn't there, or isn't for this key", keyCheckUnsure
	case ctx.Err() != nil:
		return "no answer within " + timeout.String(), keyCheckUnsure
	case errors.As(err, &exit) && exit.ExitCode() == sshConnectionError:
		return "couldn't log in: " + sshFailureText(stderr), keyCheckUnsure
	case overflow:
		// The word could have been in what wasn't kept.
		return "more output than the check reads, so it can't tell", keyCheckUnsure
	case exited && exit.ExitCode() < 0:
		return "ssh was stopped before it finished, so it can't tell", keyCheckUnsure
	case err != nil && !exited:
		return "couldn't run ssh: " + termsafe.Text(err.Error()), keyCheckUnsure
	}
	// ssh ran to its end, logged in (not 255), and the word didn't come back: the host
	// ran something else.
	return "limited: the host ran its own command for the watcher key, not the one sent", keyCheckLimited
}

// watcherKeyLine is the authorized_keys line for r's watcher key, and the account a
// look logs in to, as ssh -G works it out.
func watcherKeyLine(ctx context.Context, r config.Resolved, identity string, timeout time.Duration) (line, account string, err error) {
	if identity == "" {
		identity = r.AgentsIdentity
	}
	if identity == "" {
		return "", "", fmt.Errorf("%s has no agents_identity: make a key (`meterm keygen --file ~/.ssh/meterm_agents`), set agents_identity to it in hosts.yaml, or pass --identity", exactName(r.Name))
	}
	if config.CheckText("agents_identity", identity) != nil {
		return "", "", fmt.Errorf("the watcher key's path has characters that don't show")
	}
	key, err := readPublicKey(identity)
	if err != nil {
		return "", "", err
	}
	// What a look would refuse locally, said now rather than after the line is in.
	private := strings.TrimSuffix(identity, ".pub")
	if why := watcherKeyUnusable(private); why != "" {
		return "", "", errors.New(why)
	}
	if why := watcherKeyHasCert(private); why != "" {
		return "", "", errors.New(why)
	}
	// A key an ordinary login could use too would put those logins under the forced
	// command, or leave this line unused behind another: sshd takes the first line a
	// key matches. Either way the host wouldn't say, as the forced command is what a
	// look sends anyway.
	set, err := lookSettings(ctx, r, timeout)
	if err != nil {
		return "", "", fmt.Errorf("can't tell which keys an ordinary login to %s uses: %s", exactName(r.Name), strings.TrimPrefix(err.Error(), errWatcherSettings.Error()+": "))
	}
	others, unchecked := otherLoginKeys(ctx, set)
	// ssh -G leaves out an -i whose file isn't there, so the host's own is read here too.
	if r.IdentityFile != "" {
		if k, err := publicKeyOf(r.IdentityFile); err == nil {
			others = append(others, k)
		}
	}
	for _, o := range others {
		if sameKey(o, key) {
			return "", "", fmt.Errorf("that key can log in to %s already: it's the host's identity_file, an IdentityFile ssh tries for it, or in ssh-agent. "+
				"A watcher key has to be one of its own, kept out of ssh-agent (ssh-add -d), or the line would either stay unlimited or limit your own logins", exactName(r.Name))
		}
	}
	for _, u := range unchecked {
		fmt.Fprintf(os.Stderr, "meterm: couldn't read the public key of %s to check it isn't the same key; run --check once the line is in\n", termsafe.Quote(u))
	}
	opts, err := agents.KeyOptions()
	if err != nil {
		return "", "", err
	}
	account = set.one("user")
	if account == "" {
		account = "the account a look logs in as"
	}
	return opts + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " " + agents.KeyComment, account, nil
}

// readPublicKey reads path.pub, or path if it ends in .pub: one key, with no options.
func readPublicKey(path string) (ssh.PublicKey, error) {
	if !strings.HasSuffix(path, ".pub") {
		path += ".pub"
	}
	f, err := os.Open(sshkey.LocalPath(path))
	if err != nil {
		// Not err itself: it repeats the path as it is, and this one may come from a
		// file someone else wrote.
		why := "can't be opened"
		if os.IsNotExist(err) {
			why = "isn't there"
		}
		return nil, fmt.Errorf("can't read the public key %s: it %s", termsafe.Quote(path), why)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, err
	}
	key, _, opts, rest, err := ssh.ParseAuthorizedKey(b)
	if err != nil || len(opts) > 0 || len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("%s isn't one public key", termsafe.Quote(path))
	}
	if _, isCert := key.(*ssh.Certificate); isCert {
		return nil, fmt.Errorf("%s is a certificate; give the plain key", termsafe.Quote(path))
	}
	return key, nil
}
