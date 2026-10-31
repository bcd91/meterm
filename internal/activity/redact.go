package activity

import (
	"path"
	"regexp"
	"strings"
	"unicode"
)

// redacted replaces a value that looks like a credential.
const redacted = "[REDACTED]"

// A value on a command line: quoted, including with quotes escaped for JSON
// inside a shell string, or up to the next separator. A comma doesn't end one:
// better to mask too much than to leave the rest of a password behind.
const secretValue = `\\"(?:[^"\\]|\\[^"])*\\"|"(?:[^"\\]|\\.)*"|'[^']*'|[^\s&;]+`

// The words that name a credential, in a flag, a variable or a key.
const secretWord = `pass(?:word|wd|phrase)?|token|secret|credentials?|api[_-]?key|access[_-]?key|private[_-]?key|auth[_-]?key`

var (
	privateKeyRE = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|$)`)
	// A header in quotes goes up to its closing quote, whatever's in it.
	quotedHeaderRE = regexp.MustCompile(`(?i)(["'])((?:proxy-)?authorization|(?:set-)?cookie)(\s*[:=]\s*)[^"']*`)
	// The shell has already taken the quotes off one given to meterm exec.
	authHeaderRE = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization|(?:set-)?cookie)(\s*[:=]\s*)(?:(?:bearer|basic|token|digest)\s+)?(?:` + secretValue + `)(?:;\s*[^\s;]+)*`)
	bearerRE     = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	// Up to the last @ in the word, since a password can hold / and @. A port and
	// a path with an @ in it get masked too.
	urlPasswordRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^\s/:@]*:)\S*(@)`)
	// https://TOKEN@host. ssh://git@host is a user name, and stays.
	urlTokenRE = regexp.MustCompile(`(?i)(https?://)[^\s/:@]+(@)`)
	urlParamRE = regexp.MustCompile(`(?i)([?&](?:sig|signature|x-amz-signature|x-amz-credential|auth|key|apikey|code)=)[^&\s"']+`)
	// --password=x, PGPASSWORD=x, "token": "x", \"token\":\"x\", X-Api-Key: x
	assignmentRE = regexp.MustCompile(`(?i)(["']?[\w.-]*(?:` + secretWord + `)[\w.-]*(?:\\?["'])?\s*[:=]\s*)(` + secretValue + `)`)
	// MYSQL_PWD=x, REDISCLI_AUTH=x: a variable's ending says what it holds where
	// the word alone (PWD, KEY) would catch too much.
	secretVarRE = regexp.MustCompile(`\b([A-Z0-9_]*_(?:PWD|PASS|AUTH|KEY)\s*=\s*)(` + secretValue + `)`)
	// --password x. A flag with no value of its own keeps the next word.
	flagRE = regexp.MustCompile(`(?i)((?:^|\s)--?([\w-]*(?:` + secretWord + `)[\w-]*)\s+)(` + secretValue + `)`)
	// aws configure set aws_secret_access_key x
	awsSettingRE = regexp.MustCompile(`(?i)\b((?:aws_secret_access_key|aws_session_token|aws_access_key_id)\s+)(` + secretValue + `)`)
	// echo x | sudo -S, echo x | docker login --password-stdin
	echoedRE = regexp.MustCompile(`(\becho\s+(?:-n\s+)?)(?:` + secretValue + `)(\s*\|\s*\S*(?:sudo\s[^|;]*-\w*S\b|[^|;]*--password-stdin))`)
	// Tokens recognisable by their shape alone.
	tokenRE   = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{16,}|xox[abeoprs]-[A-Za-z0-9-]{10,}|A(?:KIA|SIA)[0-9A-Z]{16}|sk-[A-Za-z0-9_-]{20,}|[sr]k_(?:live|test)_[A-Za-z0-9]{6,}|tskey-[A-Za-z0-9-]+|npm_[A-Za-z0-9]{20,}|pypi-[A-Za-z0-9_-]{20,}|AIza[0-9A-Za-z_-]{20,}|ya29\.[0-9A-Za-z_-]{20,}|SG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}|shp(?:at|ss)_[A-Za-z0-9]{16,}|dop_v1_[a-f0-9]{16,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})\b`)
	webhookRE = regexp.MustCompile(`(?i)\b((?:hooks\.slack\.com/services|discord(?:app)?\.com/api/webhooks)/)\S+`)

	// What these mean depends on the program, so they're only read this way after
	// the programs named in programFlags. mysql's password flag has no space:
	// -pSECRET. Elsewhere -p is a port or a parent directory.
	attachedPasswordRE = regexp.MustCompile(`((?:^|\s)-p)(` + secretValue + `)`)
	spacedPasswordRE   = regexp.MustCompile(`((?:^|\s)-p\s+)(` + secretValue + `)`)
	curlUserRE         = regexp.MustCompile(`((?:^|\s)(?:-[A-Za-z]*u\s*|--user[\s=]+)[^\s:]+:)(` + secretValue + `)`)
	curlCookieRE       = regexp.MustCompile(`((?:^|\s)(?:-b|--cookie)[\s=]+)(` + secretValue + `)`)
	redisAuthRE        = regexp.MustCompile(`((?:^|\s)-a\s*)(` + secretValue + `)`)
	keygenRE           = regexp.MustCompile(`((?:^|\s)-[NP]\s*)(` + secretValue + `)`)
	userPassRE         = regexp.MustCompile(`((?:^|[\s'"])[\w.-]+:)([^\s'"|;]+)`)
	htpasswdRE         = regexp.MustCompile(`((?:^|\s)-[A-Za-z]*b[A-Za-z]*\s+\S+\s+\S+\s+)(` + secretValue + `)`)
)

// Redact masks what looks like a credential in a command or path about to go
// into the log. The log can't forget, since removing a line breaks the chain,
// and it's uploaded to the team, so a secret has to be caught before it's
// written. It leans towards masking too much, and it still recognises only known
// forms: a password passed as a bare argument to a program it doesn't know about
// stays as typed.
func Redact(detail string) string {
	if detail == "" {
		return ""
	}
	s := privateKeyRE.ReplaceAllString(detail, redacted)
	s = quotedHeaderRE.ReplaceAllString(s, "${1}${2}${3}"+redacted)
	s = authHeaderRE.ReplaceAllStringFunc(s, func(m string) string {
		if strings.Contains(m, redacted) { // already masked up to its closing quote
			return m
		}
		sub := authHeaderRE.FindStringSubmatch(m)
		return sub[1] + sub[2] + redacted
	})
	s = bearerRE.ReplaceAllString(s, "${1}"+redacted)
	s = urlPasswordRE.ReplaceAllString(s, "${1}"+redacted+"${2}")
	s = urlTokenRE.ReplaceAllString(s, "${1}"+redacted+"${2}")
	s = urlParamRE.ReplaceAllString(s, "${1}"+redacted)
	s = tokenRE.ReplaceAllString(s, redacted)
	s = webhookRE.ReplaceAllString(s, "${1}"+redacted)
	s = echoedRE.ReplaceAllString(s, "${1}"+redacted+"${2}")
	s = awsSettingRE.ReplaceAllString(s, "${1}"+redacted)
	s = secretVarRE.ReplaceAllString(s, "${1}"+redacted)
	s = assignmentRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := assignmentRE.FindStringSubmatch(m)
		if pointsAtSecret(sub[1]) || sub[2] == redacted {
			return m
		}
		return sub[1] + redacted
	})
	s = flagRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := flagRE.FindStringSubmatch(m)
		if valuelessFlag(sub[2]) || strings.HasPrefix(sub[3], "-") || sub[3] == redacted {
			return m
		}
		return sub[1] + redacted
	})
	return programFlags(s)
}

// programFlags masks the flags that only mean a secret to certain programs.
func programFlags(s string) string {
	words := commandWords(s)
	has := func(names ...string) bool {
		for _, n := range names {
			if words[n] {
				return true
			}
		}
		return false
	}
	mask := func(re *regexp.Regexp) { s = re.ReplaceAllString(s, "${1}"+redacted) }
	for w := range words {
		if strings.HasPrefix(w, "mysql") || strings.HasPrefix(w, "mariadb") {
			mask(attachedPasswordRE)
			break
		}
	}
	if has("sshpass") {
		mask(attachedPasswordRE)
		mask(spacedPasswordRE)
	}
	if words["login"] && has("docker", "podman", "nerdctl", "helm", "skopeo", "buildah") {
		mask(spacedPasswordRE)
	}
	if has("curl") {
		mask(curlUserRE)
		mask(curlCookieRE)
	}
	if has("redis-cli", "valkey-cli") {
		mask(redisAuthRE)
	}
	if has("ssh-keygen") {
		mask(keygenRE)
	}
	if has("chpasswd") {
		mask(userPassRE)
	}
	if has("htpasswd") {
		mask(htpasswdRE)
	}
	return s
}

// pointsAtSecret is true for a name whose value says where the secret is, not
// what it is: --password-file=/run/pw.
func pointsAtSecret(name string) bool {
	name = strings.ToLower(strings.TrimRight(name, " \t:=\"'\\"))
	for _, suffix := range []string{"-file", "_file", "-path", "_path", "-stdin", "_stdin"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// valuelessFlag is a credential flag that doesn't take the next word as a
// secret, like docker's --password-stdin or --no-password.
func valuelessFlag(name string) bool {
	name = strings.ToLower(name)
	return strings.HasPrefix(name, "no-") || pointsAtSecret(name)
}

// commandWords is the base name of every word in s, split at the shell's own
// punctuation too, so a tool is found behind sudo, a full path, sh -c, a ; or $(.
func commandWords(s string) map[string]bool {
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("\"'`;|&()$<>", r)
	}) {
		if !strings.HasPrefix(w, "-") {
			words[path.Base(w)] = true
		}
	}
	return words
}
