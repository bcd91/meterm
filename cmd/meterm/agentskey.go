package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"meterm/internal/agents"
	"meterm/internal/config"
	"meterm/internal/sshkey"
	"meterm/internal/termsafe"
)

func newAgentsKeyCmd() *cobra.Command {
	var identity string
	cmd := &cobra.Command{
		Use:   "key <host> [--identity file]",
		Short: "Print the authorized_keys line that limits a host's watcher key to `claude agents --json`",
		Long: "Prints one line for the host's authorized_keys: the public half of its\n" +
			"agents_identity (or --identity), with a forced command that runs only\n" +
			"`claude agents --json` and `restrict`, so the key can't open a shell, a pty or a\n" +
			"forward. It doesn't install anything or contact the host: put the line there\n" +
			"yourself, ideally in an AuthorizedKeysFile the account can't write.\n\n" +
			"The limit is on the host's side because nothing on this machine can be: anything\n" +
			"running as you here, an agent included, can read the key and use it as it likes.\n\n" +
			"To set one up:\n" +
			"  meterm keygen --file ~/.ssh/meterm_agents\n" +
			"  # in hosts.yaml, on the host, its group or defaults: agents_identity: ~/.ssh/meterm_agents\n" +
			"  meterm agents key devbox     # then add the line on devbox\n\n" +
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
			h, idx, err := cfg.Target(args[0])
			if err != nil {
				return err
			}
			if idx < 0 {
				return fmt.Errorf("no such host: %s", exactName(args[0]))
			}
			r := cfg.Resolve(*h)
			line, err := watcherKeyLine(r, identity)
			if err != nil {
				return err
			}
			account := "the account meterm logs in as"
			if r.User != "" {
				account = lbl(r.User)
			}
			fmt.Fprintf(os.Stderr, "Add this line to the authorized_keys of %s on %s; meterm doesn't install it:\n", account, lbl(r.Name))
			_, err = fmt.Fprintln(os.Stdout, line)
			return err
		},
	}
	cmd.Flags().StringVar(&identity, "identity", "", "the watcher key (private or .pub), instead of the host's agents_identity")
	return cmd
}

// watcherKeyLine is the authorized_keys line for r's watcher key.
func watcherKeyLine(r config.Resolved, identity string) (string, error) {
	if identity == "" {
		identity = r.AgentsIdentity
	}
	if identity == "" {
		return "", fmt.Errorf("%s has no agents_identity: make a key (`meterm keygen --file ~/.ssh/meterm_agents`), set agents_identity to it in hosts.yaml, or pass --identity", exactName(r.Name))
	}
	key, err := readPublicKey(identity)
	if err != nil {
		return "", err
	}
	// The same key as the host's other logins would put those under the forced
	// command too, or leave this line unused behind the other one: sshd takes the
	// first line a key matches.
	if r.IdentityFile != "" {
		if other, err := readPublicKey(r.IdentityFile); err == nil && bytes.Equal(other.Marshal(), key.Marshal()) {
			return "", fmt.Errorf("that's the key %s's identity_file is, too: a watcher key has to be one of its own", exactName(r.Name))
		}
	}
	opts, err := agents.KeyOptions()
	if err != nil {
		return "", err
	}
	return opts + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " " + agents.KeyComment, nil
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
