package agents

import (
	"errors"
	"strings"
)

// The watcher key: a key that can only ask `claude agents --json`, because the host
// says so. The limit is on the host's side on purpose. On this machine, anything that
// runs as the same user (an agent included) can read the key and run ssh with it as
// it likes; what it can't change is the line in the host's authorized_keys, so that
// line is where "only this command" has to be.

// KeyOptions are the authorized_keys options for a watcher key, in front of the key.
// restrict is the one that matters, and covers everything else that sshd knows of now
// and later; the no-* ones say the same for anyone reading the line, and for an sshd
// too old to know restrict, which then refuses the whole line and so the key: closed,
// not open. The forced command is RemoteCommand itself, so a look sends what the host
// would run anyway, and the key works the same with the line or without it. sshd runs
// the forced command whatever the client asked for, and the command doesn't read
// SSH_ORIGINAL_COMMAND, so nothing the client sends gets into it.
func KeyOptions() (string, error) {
	cmd := RemoteCommand
	// authorized_keys takes \" for a quote inside command="…" and nothing else; a
	// backslash or a line break in the command would be read some other way.
	if strings.ContainsAny(cmd, "\\\n\r\x00") || strings.Contains(cmd, "SSH_ORIGINAL_COMMAND") {
		return "", errors.New("the remote command can't go in an authorized_keys line as it is")
	}
	quoted := strings.ReplaceAll(cmd, `"`, `\"`)
	return `restrict,no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding,no-user-rc,command="` + quoted + `"`, nil
}

// KeyComment is the comment the line ends with, so `meterm keys list` shows what the
// key is for, and nothing about this machine or its user goes to the host.
const KeyComment = "meterm-agents-watcher"
