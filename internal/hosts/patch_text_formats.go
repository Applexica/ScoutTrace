package hosts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type externalMarkers struct {
	Servers map[string]ServerOriginal `json:"servers"`
}

func patchBytesTOML(in []byte, serversKey string, servers []string, proxyExe string, markerDir string) ([]byte, error) {
	out, _, err := patchBytesTOMLNamed(in, serversKey, servers, proxyExe, markerDir)
	return out, err
}

func patchBytesTOMLNamed(in []byte, serversKey string, servers []string, proxyExe string, markerDir string) ([]byte, []string, error) {
	parsed, err := loadTOMLServers(in, serversKey)
	if err != nil {
		return nil, nil, err
	}
	want := wantedServers(servers)
	lines := splitLinesPreserve(in)
	sectionRe := regexp.MustCompile(`^\s*\[` + regexp.QuoteMeta(serversKey) + `\.([A-Za-z0-9_.-]+)\]\s*$`)
	markers := externalMarkers{Servers: map[string]ServerOriginal{}}
	var patched []string
	for i := 0; i < len(lines); i++ {
		m := sectionRe.FindStringSubmatch(strings.TrimRight(lines[i], "\r\n"))
		if m == nil {
			continue
		}
		name := m[1]
		if len(want) > 0 && !want[name] {
			continue
		}
		entry := parsed[name]
		if entry.Command == "" || isManaged(entry.Command, entry.Args) {
			continue
		}
		end := nextTOMLSection(lines, i+1)
		cmdIdx, argsIdx := -1, -1
		for j := i + 1; j < end; j++ {
			trim := strings.TrimSpace(lines[j])
			if strings.HasPrefix(trim, "command") && strings.Contains(trim, "=") {
				cmdIdx = j
			}
			if strings.HasPrefix(trim, "args") && strings.Contains(trim, "=") {
				argsIdx = j
			}
		}
		if cmdIdx == -1 {
			return nil, nil, fmt.Errorf("hosts: server %q missing command", name)
		}
		if argsIdx == -1 {
			return nil, nil, fmt.Errorf("hosts: server %q missing args", name)
		}
		markers.Servers[name] = ServerOriginal{Command: entry.Command, Args: entry.Args}
		lines[cmdIdx] = linePrefix(lines[cmdIdx]) + "command = " + quoteString(proxyExe) + lineEnding(lines[cmdIdx])
		newArgs := append([]string{"proxy", "--server-name", name, "--", entry.Command}, entry.Args...)
		lines[argsIdx] = linePrefix(lines[argsIdx]) + "args = " + tomlStringArray(newArgs) + lineEnding(lines[argsIdx])
		patched = append(patched, name)
	}
	if len(patched) > 0 {
		if err := writeExternalMarkers(markerDir, markers); err != nil {
			return nil, nil, err
		}
		sort.Strings(patched)
	}
	return []byte(strings.Join(lines, "")), patched, nil
}

func unpatchBytesTOML(in []byte, serversKey string, markerDir string) ([]byte, []string, error) {
	markers, err := readExternalMarkers(markerDir)
	if err != nil {
		return nil, nil, err
	}
	lines := splitLinesPreserve(in)
	sectionRe := regexp.MustCompile(`^\s*\[` + regexp.QuoteMeta(serversKey) + `\.([A-Za-z0-9_.-]+)\]\s*$`)
	var restored []string
	for i := 0; i < len(lines); i++ {
		m := sectionRe.FindStringSubmatch(strings.TrimRight(lines[i], "\r\n"))
		if m == nil {
			continue
		}
		name := m[1]
		orig, ok := markers.Servers[name]
		if !ok {
			continue
		}
		end := nextTOMLSection(lines, i+1)
		cmdIdx, argsIdx := -1, -1
		for j := i + 1; j < end; j++ {
			trim := strings.TrimSpace(lines[j])
			if strings.HasPrefix(trim, "command") && strings.Contains(trim, "=") {
				cmdIdx = j
			}
			if strings.HasPrefix(trim, "args") && strings.Contains(trim, "=") {
				argsIdx = j
			}
		}
		if cmdIdx >= 0 {
			lines[cmdIdx] = linePrefix(lines[cmdIdx]) + "command = " + quoteString(orig.Command) + lineEnding(lines[cmdIdx])
		}
		if argsIdx >= 0 {
			lines[argsIdx] = linePrefix(lines[argsIdx]) + "args = " + tomlStringArray(orig.Args) + lineEnding(lines[argsIdx])
		}
		restored = append(restored, name)
	}
	sort.Strings(restored)
	return []byte(strings.Join(lines, "")), restored, nil
}

func loadTOMLServers(in []byte, serversKey string) (map[string]ServerOriginal, error) {
	servers := map[string]ServerOriginal{}
	lines := splitLinesPreserve(in)
	sectionRe := regexp.MustCompile(`^\s*\[` + regexp.QuoteMeta(serversKey) + `\.([A-Za-z0-9_.-]+)\]\s*$`)
	current := ""
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasPrefix(trim, "[") {
			if m := sectionRe.FindStringSubmatch(strings.TrimRight(line, "\r\n")); m != nil {
				current = m[1]
				if _, ok := servers[current]; !ok {
					servers[current] = ServerOriginal{}
				}
			} else {
				current = ""
			}
			continue
		}
		if current == "" || !strings.Contains(trim, "=") {
			continue
		}
		parts := strings.SplitN(trim, "=", 2)
		key, val := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		entry := servers[current]
		switch key {
		case "command":
			cmd, err := strconv.Unquote(val)
			if err != nil {
				return nil, err
			}
			entry.Command = cmd
		case "args":
			var args []string
			if err := json.Unmarshal([]byte(val), &args); err != nil {
				return nil, err
			}
			entry.Args = args
		}
		servers[current] = entry
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("hosts: %q key absent", serversKey)
	}
	return servers, nil
}

func patchBytesYAML(in []byte, serversKey string, servers []string, proxyExe string, markerDir string) ([]byte, error) {
	out, _, err := patchBytesYAMLNamed(in, serversKey, servers, proxyExe, markerDir)
	return out, err
}

func patchBytesYAMLNamed(in []byte, serversKey string, servers []string, proxyExe string, markerDir string) ([]byte, []string, error) {
	parsed, err := loadYAMLServers(in, serversKey)
	if err != nil {
		return nil, nil, err
	}
	want := wantedServers(servers)
	lines := splitLinesPreserve(in)
	ranges, rootIndent, err := yamlServerRanges(lines, serversKey)
	if err != nil {
		return nil, nil, err
	}
	markers := externalMarkers{Servers: map[string]ServerOriginal{}}
	var patched []string
	for idx := len(ranges) - 1; idx >= 0; idx-- {
		r := ranges[idx]
		name := r.name
		if len(want) > 0 && !want[name] {
			continue
		}
		entry := parsed[name]
		if entry.Command == "" || isManaged(entry.Command, entry.Args) {
			continue
		}
		cmdIdx, argsIdx := -1, -1
		argsEnd := -1
		for j := r.start + 1; j < r.end; j++ {
			trimj := strings.TrimSpace(lines[j])
			if countIndent(lines[j]) == rootIndent+4 && strings.HasPrefix(trimj, "command:") {
				cmdIdx = j
			}
			if countIndent(lines[j]) == rootIndent+4 && strings.HasPrefix(trimj, "args:") {
				argsIdx = j
				argsEnd = nextYAMLField(lines, j+1, rootIndent+4, r.end)
			}
		}
		if cmdIdx == -1 || argsIdx == -1 {
			return nil, nil, fmt.Errorf("hosts: server %q missing command or args", name)
		}
		markers.Servers[name] = ServerOriginal{Command: entry.Command, Args: entry.Args}
		lines[cmdIdx] = strings.Repeat(" ", rootIndent+4) + "command: " + yamlScalar(proxyExe) + lineEnding(lines[cmdIdx])
		newArgs := append([]string{"proxy", "--server-name", name, "--", entry.Command}, entry.Args...)
		repl := []string{strings.Repeat(" ", rootIndent+4) + "args:" + lineEnding(lines[argsIdx])}
		for _, arg := range newArgs {
			repl = append(repl, strings.Repeat(" ", rootIndent+6)+"- "+quoteString(arg)+lineEnding(lines[argsIdx]))
		}
		lines = replaceLines(lines, argsIdx, argsEnd, repl)
		patched = append(patched, name)
	}
	if len(patched) > 0 {
		if err := writeExternalMarkers(markerDir, markers); err != nil {
			return nil, nil, err
		}
		sort.Strings(patched)
	}
	return []byte(strings.Join(lines, "")), patched, nil
}

func unpatchBytesYAML(in []byte, serversKey string, markerDir string) ([]byte, []string, error) {
	markers, err := readExternalMarkers(markerDir)
	if err != nil {
		return nil, nil, err
	}
	lines := splitLinesPreserve(in)
	ranges, rootIndent, err := yamlServerRanges(lines, serversKey)
	if err != nil {
		return nil, nil, err
	}
	var restored []string
	for idx := len(ranges) - 1; idx >= 0; idx-- {
		r := ranges[idx]
		name := r.name
		orig, ok := markers.Servers[name]
		if !ok {
			continue
		}
		cmdIdx, argsIdx := -1, -1
		argsEnd := -1
		for j := r.start + 1; j < r.end; j++ {
			trimj := strings.TrimSpace(lines[j])
			if countIndent(lines[j]) == rootIndent+4 && strings.HasPrefix(trimj, "command:") {
				cmdIdx = j
			}
			if countIndent(lines[j]) == rootIndent+4 && strings.HasPrefix(trimj, "args:") {
				argsIdx = j
				argsEnd = nextYAMLField(lines, j+1, rootIndent+4, r.end)
			}
		}
		if cmdIdx >= 0 {
			lines[cmdIdx] = strings.Repeat(" ", rootIndent+4) + "command: " + yamlScalar(orig.Command) + lineEnding(lines[cmdIdx])
		}
		if argsIdx >= 0 {
			repl := []string{strings.Repeat(" ", rootIndent+4) + "args:" + lineEnding(lines[argsIdx])}
			for _, arg := range orig.Args {
				repl = append(repl, strings.Repeat(" ", rootIndent+6)+"- "+quoteString(arg)+lineEnding(lines[argsIdx]))
			}
			lines = replaceLines(lines, argsIdx, argsEnd, repl)
		}
		restored = append(restored, name)
	}
	sort.Strings(restored)
	return []byte(strings.Join(lines, "")), restored, nil
}

func loadYAMLServers(in []byte, serversKey string) (map[string]ServerOriginal, error) {
	lines := splitLinesPreserve(in)
	ranges, rootIndent, err := yamlServerRanges(lines, serversKey)
	if err != nil {
		return nil, err
	}
	servers := map[string]ServerOriginal{}
	for _, r := range ranges {
		entry := ServerOriginal{}
		for j := r.start + 1; j < r.end; j++ {
			trimj := strings.TrimSpace(lines[j])
			if countIndent(lines[j]) == rootIndent+4 && strings.HasPrefix(trimj, "command:") {
				entry.Command = parseYAMLScalar(strings.TrimSpace(strings.TrimPrefix(trimj, "command:")))
			}
			if countIndent(lines[j]) == rootIndent+4 && strings.HasPrefix(trimj, "args:") {
				argsEnd := nextYAMLField(lines, j+1, rootIndent+4, r.end)
				for k := j + 1; k < argsEnd; k++ {
					tr := strings.TrimSpace(lines[k])
					if strings.HasPrefix(tr, "- ") {
						entry.Args = append(entry.Args, parseYAMLScalar(strings.TrimSpace(strings.TrimPrefix(tr, "- "))))
					}
				}
			}
		}
		servers[r.name] = entry
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("hosts: %q key absent", serversKey)
	}
	return servers, nil
}

func wantedServers(servers []string) map[string]bool {
	want := map[string]bool{}
	for _, s := range servers {
		if strings.TrimSpace(s) != "" {
			want[strings.TrimSpace(s)] = true
		}
	}
	return want
}

func isManaged(command string, args []string) bool {
	return len(args) >= 4 && args[0] == "proxy" && args[1] == "--server-name" && args[3] == "--"
}

func writeExternalMarkers(dir string, markers externalMarkers) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(markers, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(filepath.Join(dir, "markers.json"), b, 0o600)
}

func readExternalMarkers(dir string) (externalMarkers, error) {
	var markers externalMarkers
	b, err := os.ReadFile(filepath.Join(dir, "markers.json"))
	if err != nil {
		return markers, err
	}
	if err := json.Unmarshal(b, &markers); err != nil {
		return markers, err
	}
	if markers.Servers == nil {
		markers.Servers = map[string]ServerOriginal{}
	}
	return markers, nil
}

func splitLinesPreserve(in []byte) []string {
	if len(in) == 0 {
		return nil
	}
	parts := bytes.SplitAfter(in, []byte("\n"))
	lines := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) > 0 {
			lines = append(lines, string(p))
		}
	}
	return lines
}

func replaceLines(lines []string, start, end int, repl []string) []string {
	out := make([]string, 0, len(lines)-(end-start)+len(repl))
	out = append(out, lines[:start]...)
	out = append(out, repl...)
	out = append(out, lines[end:]...)
	return out
}

func nextTOMLSection(lines []string, start int) int {
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			return i
		}
	}
	return len(lines)
}

func lineEnding(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return "\r\n"
	}
	if strings.HasSuffix(s, "\n") {
		return "\n"
	}
	return ""
}

func linePrefix(s string) string {
	idx := strings.IndexFunc(s, func(r rune) bool { return r != ' ' && r != '\t' })
	if idx < 0 {
		return ""
	}
	return s[:idx]
}

func quoteString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func tomlStringArray(args []string) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = quoteString(arg)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

type yamlRange struct {
	name       string
	start, end int
}

func yamlServerRanges(lines []string, serversKey string) ([]yamlRange, int, error) {
	rootIdx, rootIndent, err := findYAMLRoot(lines, serversKey)
	if err != nil {
		return nil, 0, err
	}
	var starts []yamlRange
	closeIdx := len(lines)
	for i := rootIdx + 1; i < len(lines); i++ {
		indent := countIndent(lines[i])
		trim := strings.TrimSpace(lines[i])
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if indent <= rootIndent {
			closeIdx = i
			break
		}
		if indent == rootIndent+2 && strings.HasSuffix(trim, ":") {
			starts = append(starts, yamlRange{name: strings.TrimSuffix(trim, ":"), start: i})
		}
	}
	for i := range starts {
		if i+1 < len(starts) {
			starts[i].end = starts[i+1].start
		} else {
			starts[i].end = closeIdx
		}
	}
	return starts, rootIndent, nil
}

func findYAMLRoot(lines []string, key string) (int, int, error) {
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.TrimSuffix(trim, ":") == key && strings.HasSuffix(trim, ":") {
			return i, countIndent(line), nil
		}
	}
	return -1, 0, fmt.Errorf("hosts: %q key absent", key)
}

func countIndent(line string) int {
	n := 0
	for _, r := range line {
		if r == ' ' {
			n++
			continue
		}
		break
	}
	return n
}

func nextYAMLPeer(lines []string, start int, indent int) int {
	for i := start; i < len(lines); i++ {
		trim := strings.TrimSpace(lines[i])
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		lineIndent := countIndent(lines[i])
		if lineIndent < indent {
			return i
		}
		if lineIndent == indent && strings.HasSuffix(trim, ":") {
			return i
		}
	}
	return len(lines)
}

func nextYAMLField(lines []string, start int, indent int, end int) int {
	for i := start; i < end; i++ {
		trim := strings.TrimSpace(lines[i])
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if countIndent(lines[i]) <= indent && strings.HasSuffix(trim, ":") {
			return i
		}
	}
	return end
}

func yamlScalar(s string) string {
	if regexp.MustCompile(`^[A-Za-z0-9_./]+[A-Za-z0-9_./-]*$`).MatchString(s) && !strings.HasPrefix(s, "-") && !strings.HasPrefix(s, "@") {
		return s
	}
	return quoteString(s)
}

func parseYAMLScalar(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if (strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"")) || (strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'")) {
		if unq, err := strconv.Unquote(s); err == nil {
			return unq
		}
		return strings.Trim(s, "'")
	}
	return s
}
