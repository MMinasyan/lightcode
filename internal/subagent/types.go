package subagent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type AgentType struct {
	Name        string
	Description string
	Tools       []string
	Prompt      string
}

type Loader struct {
	projectRoot string
	home        string
}

func NewLoader(projectRoot, home string) *Loader {
	return &Loader{projectRoot: projectRoot, home: home}
}

func (l *Loader) Load(name string) (AgentType, error) {
	// Project-level overrides user-level overrides built-in.
	if at, err := l.loadFromDir(filepath.Join(l.projectRoot, ".lightcode", "agents"), name); err == nil {
		return at, nil
	}
	if at, err := l.loadFromDir(filepath.Join(l.home, ".lightcode", "agents"), name); err == nil {
		return at, nil
	}
	if at, err := l.loadBuiltin(name); err == nil {
		return at, nil
	}
	return AgentType{}, fmt.Errorf("unknown subagent type %q", name)
}

func (l *Loader) All() []AgentType {
	seen := map[string]bool{}
	var result []AgentType

	add := func(at AgentType) {
		if seen[at.Name] {
			return
		}
		seen[at.Name] = true
		result = append(result, at)
	}

	if types, err := l.loadAllFromDir(filepath.Join(l.projectRoot, ".lightcode", "agents")); err == nil {
		for _, at := range types {
			add(at)
		}
	}
	if types, err := l.loadAllFromDir(filepath.Join(l.home, ".lightcode", "agents")); err == nil {
		for _, at := range types {
			add(at)
		}
	}
	if types, err := l.loadAllBuiltin(); err == nil {
		for _, at := range types {
			add(at)
		}
	}
	return result
}

func (l *Loader) loadFromDir(dir, name string) (AgentType, error) {
	data, err := os.ReadFile(filepath.Join(dir, name+".md"))
	if err != nil {
		return AgentType{}, err
	}
	return parseAgentFile(string(data))
}

func (l *Loader) loadAllFromDir(dir string) ([]AgentType, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var result []AgentType
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		at, err := parseAgentFile(string(data))
		if err != nil {
			continue
		}
		result = append(result, at)
	}
	return result, nil
}

func (l *Loader) loadBuiltin(name string) (AgentType, error) {
	data, err := builtinFS.ReadFile("types/" + name + ".md")
	if err != nil {
		return AgentType{}, err
	}
	return parseAgentFile(string(data))
}

func (l *Loader) loadAllBuiltin() ([]AgentType, error) {
	entries, err := builtinFS.ReadDir("types")
	if err != nil {
		return nil, err
	}
	var result []AgentType
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := builtinFS.ReadFile("types/" + e.Name())
		if err != nil {
			continue
		}
		at, err := parseAgentFile(string(data))
		if err != nil {
			continue
		}
		result = append(result, at)
	}
	return result, nil
}

func parseAgentFile(content string) (AgentType, error) {
	fm, body := parseFrontmatter(content)
	name, _ := fm["name"].(string)
	if name == "" {
		return AgentType{}, fmt.Errorf("missing name in frontmatter")
	}
	desc, _ := fm["description"].(string)
	var tools []string
	if t, ok := fm["tools"]; ok {
		if list, ok := t.([]string); ok {
			tools = list
		}
	}
	return AgentType{
		Name:        name,
		Description: desc,
		Tools:       tools,
		Prompt:      strings.TrimSpace(body),
	}, nil
}

func parseFrontmatter(content string) (map[string]any, string) {
	if !strings.HasPrefix(content, "---\n") {
		return nil, content
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, content
	}
	fmBlock := rest[:end]
	body := rest[end+4:]

	result := map[string]any{}
	var listKey string
	var listItems []string

	flushList := func() {
		if listKey != "" {
			result[listKey] = listItems
			listKey = ""
			listItems = nil
		}
	}

	for _, line := range strings.Split(fmBlock, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(line, "  - ") || strings.HasPrefix(line, "\t- ") {
			if listKey != "" {
				listItems = append(listItems, strings.TrimSpace(line[4:]))
			}
			continue
		}
		flushList()
		idx := strings.Index(trimmed, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:idx])
		val := strings.TrimSpace(trimmed[idx+1:])
		if val == "" {
			listKey = key
			listItems = nil
		} else {
			result[key] = val
		}
	}
	flushList()
	return result, body
}
