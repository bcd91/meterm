package inventory

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"meterm/internal/config"
)

// ansibleInv is an inventory in either format, flattened to groups and hosts.
type ansibleInv struct {
	groups   map[string]*ansibleGroup
	hostVars map[string]map[string]string
	order    []string // hosts in the order first seen
}

type ansibleGroup struct {
	hosts    []string
	children []string
	vars     map[string]string
}

func newInv() *ansibleInv {
	return &ansibleInv{groups: map[string]*ansibleGroup{}, hostVars: map[string]map[string]string{}}
}

func (inv *ansibleInv) group(name string) *ansibleGroup {
	g, ok := inv.groups[name]
	if !ok {
		g = &ansibleGroup{vars: map[string]string{}}
		inv.groups[name] = g
	}
	return g
}

func (inv *ansibleInv) addHost(group, name string, vars map[string]string) {
	if _, ok := inv.hostVars[name]; !ok {
		inv.hostVars[name] = map[string]string{}
		inv.order = append(inv.order, name)
	}
	for k, v := range vars {
		inv.hostVars[name][k] = v
	}
	if group != "" {
		g := inv.group(group)
		g.hosts = append(g.hosts, name)
	}
}

// FromAnsible reads an Ansible inventory, INI or YAML. Each host keeps its
// inventory name; ansible_host/user/port/ssh_private_key_file and a -J or
// ProxyJump in ansible_ssh_common_args carry over, with group vars applied
// beneath host vars; group names become tags. Host ranges (web[01:20]) are
// not expanded and are returned in skipped.
func FromAnsible(data []byte) (hosts []config.Host, skipped []string, err error) {
	var inv *ansibleInv
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("[")) || !looksYAML(trimmed) {
		inv, skipped = parseAnsibleINI(data)
	} else {
		inv, skipped, err = parseAnsibleYAML(data)
		if err != nil {
			return nil, nil, err
		}
	}
	return inv.hosts(), skipped, nil
}

func looksYAML(b []byte) bool {
	var m map[string]any
	return yaml.Unmarshal(b, &m) == nil && len(m) > 0 && !bytes.Contains(b, []byte("\n["))
}

func parseAnsibleINI(data []byte) (*ansibleInv, []string) {
	inv := newInv()
	var skipped []string
	section, kind := "ungrouped", "hosts"
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := line[1 : len(line)-1]
			section, kind = name, "hosts"
			if g, k, ok := strings.Cut(name, ":"); ok {
				section, kind = g, k
			}
			inv.group(section)
			continue
		}
		switch kind {
		case "vars":
			if k, v, ok := strings.Cut(line, "="); ok {
				inv.group(section).vars[strings.TrimSpace(k)] = unquote(strings.TrimSpace(v))
			}
		case "children":
			g := inv.group(section)
			g.children = append(g.children, strings.Fields(line)[0])
			inv.group(strings.Fields(line)[0])
		default:
			fields := splitINIFields(line)
			name := fields[0]
			if strings.ContainsAny(name, "[]") {
				skipped = append(skipped, name)
				continue
			}
			vars := map[string]string{}
			for _, f := range fields[1:] {
				if k, v, ok := strings.Cut(f, "="); ok {
					vars[k] = unquote(v)
				}
			}
			inv.addHost(section, name, vars)
		}
	}
	return inv, skipped
}

// splitINIFields splits on spaces outside quotes, so
// ansible_ssh_common_args='-J ops@bastion' stays one field.
func splitINIFields(line string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
			cur.WriteRune(r)
		case r == ' ' || r == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func parseAnsibleYAML(data []byte) (*ansibleInv, []string, error) {
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, err
	}
	inv := newInv()
	var skipped []string
	var walk func(name string, node any)
	walk = func(name string, node any) {
		g := inv.group(name)
		m, _ := node.(map[string]any)
		if vars, ok := m["vars"].(map[string]any); ok {
			for k, v := range vars {
				g.vars[k] = scalar(v)
			}
		}
		if hosts, ok := m["hosts"].(map[string]any); ok {
			for h, hv := range hosts {
				if strings.ContainsAny(h, "[]") {
					skipped = append(skipped, h)
					continue
				}
				vars := map[string]string{}
				if hm, ok := hv.(map[string]any); ok {
					for k, v := range hm {
						vars[k] = scalar(v)
					}
				}
				inv.addHost(name, h, vars)
			}
		}
		if children, ok := m["children"].(map[string]any); ok {
			names := make([]string, 0, len(children))
			for c := range children {
				names = append(names, c)
			}
			sort.Strings(names)
			for _, c := range names {
				g.children = append(g.children, c)
				walk(c, children[c])
			}
		}
	}
	names := make([]string, 0, len(root))
	for k := range root {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		walk(k, root[k])
	}
	sort.Strings(inv.order) // YAML maps have no order
	return inv, skipped, nil
}

func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

var jumpRe = regexp.MustCompile(`(?:-J\s*|ProxyJump=)(\S+)`)

// hosts resolves each host's vars (group vars from the outermost group in,
// then the host's own) and builds meterm hosts.
func (inv *ansibleInv) hosts() []config.Host {
	parents := map[string][]string{} // group -> groups that list it as a child
	for name, g := range inv.groups {
		for _, c := range g.children {
			parents[c] = append(parents[c], name)
		}
	}
	// depth orders groups so a child group's vars beat its parent's.
	var depth func(string, map[string]bool) int
	depth = func(g string, seen map[string]bool) int {
		if seen[g] {
			return 0
		}
		seen[g] = true
		d := 0
		for _, p := range parents[g] {
			d = max(d, depth(p, seen)+1)
		}
		return d
	}
	var out []config.Host
	for _, name := range inv.order {
		member := map[string]bool{}
		var add func(string)
		add = func(g string) {
			if member[g] {
				return
			}
			member[g] = true
			for _, p := range parents[g] {
				add(p)
			}
		}
		for gname, g := range inv.groups {
			for _, h := range g.hosts {
				if h == name {
					add(gname)
				}
			}
		}
		groups := make([]string, 0, len(member)+1)
		for g := range member {
			groups = append(groups, g)
		}
		if !member["all"] {
			groups = append(groups, "all")
		}
		rank := func(g string) int {
			if g == "all" {
				return -1 // the weakest vars, whatever the file says
			}
			return depth(g, map[string]bool{})
		}
		sort.Slice(groups, func(i, j int) bool {
			if ri, rj := rank(groups[i]), rank(groups[j]); ri != rj {
				return ri < rj
			}
			return groups[i] < groups[j]
		})
		vars := map[string]string{}
		var tags []string
		for _, g := range groups {
			if gr, ok := inv.groups[g]; ok {
				for k, v := range gr.vars {
					vars[k] = v
				}
			}
			if g != "all" && g != "ungrouped" {
				tags = append(tags, g)
			}
		}
		for k, v := range inv.hostVars[name] {
			vars[k] = v
		}
		sort.Strings(tags)
		h := config.Host{
			Name:         name,
			Address:      first(vars["ansible_host"], vars["ansible_ssh_host"], name),
			User:         first(vars["ansible_user"], vars["ansible_ssh_user"]),
			IdentityFile: vars["ansible_ssh_private_key_file"],
			Tags:         tags,
		}
		if p, err := strconv.Atoi(first(vars["ansible_port"], vars["ansible_ssh_port"])); err == nil && p != 22 {
			h.Port = p
		}
		if m := jumpRe.FindStringSubmatch(vars["ansible_ssh_common_args"]); m != nil {
			h.ProxyJump = strings.Trim(m[1], `'"`)
		}
		out = append(out, h)
	}
	return out
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

var notIdent = regexp.MustCompile(`[^A-Za-z0-9_]`)

// groupName makes a tag or meterm group usable as an Ansible group name.
func groupName(s string) string {
	s = notIdent.ReplaceAllString(s, "_")
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		s = "g_" + s
	}
	return s
}

// ToAnsible renders hosts as a YAML inventory: every host under all.hosts
// with its effective connection settings, and one group per meterm group
// and per tag listing its members.
func ToAnsible(cfg *config.Config) ([]byte, error) {
	type hostVars map[string]any
	all := map[string]hostVars{}
	groups := map[string]map[string]hostVars{}
	for _, h := range cfg.SortedHosts() {
		r := cfg.Resolve(h)
		v := hostVars{"ansible_host": r.Address}
		if r.User != "" {
			v["ansible_user"] = r.User
		}
		if r.Port != 0 && r.Port != 22 {
			v["ansible_port"] = r.Port
		}
		if r.IdentityFile != "" {
			v["ansible_ssh_private_key_file"] = r.IdentityFile
		}
		if j := jumpSpec(r); j != "" {
			v["ansible_ssh_common_args"] = "-J " + j
		}
		all[h.Name] = v
		var gs []string
		if h.Group != "" {
			gs = append(gs, h.Group)
		}
		gs = append(gs, r.Tags...)
		for _, g := range gs {
			g = groupName(g)
			if groups[g] == nil {
				groups[g] = map[string]hostVars{}
			}
			groups[g][h.Name] = hostVars{}
		}
	}
	children := map[string]any{}
	for g, hs := range groups {
		children[g] = map[string]any{"hosts": hs}
	}
	doc := map[string]any{"all": map[string]any{"hosts": all, "children": children}}
	var buf bytes.Buffer
	buf.WriteString("# Generated by `meterm export --format ansible`; re-run after changing hosts.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func jumpSpec(r config.Resolved) string {
	var hops []string
	for _, jh := range r.ProxyJump {
		hop := jh.Address
		if jh.User != "" {
			hop = jh.User + "@" + hop
		}
		if jh.Port != 0 && jh.Port != 22 {
			hop = fmt.Sprintf("%s:%d", hop, jh.Port)
		}
		hops = append(hops, hop)
	}
	if r.ProxyJumpRaw != "" {
		hops = append(hops, r.ProxyJumpRaw)
	}
	return strings.Join(hops, ",")
}
