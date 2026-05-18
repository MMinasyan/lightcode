package tool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
)

type ProcessController interface {
	Read(id string) (string, error)
	Kill(id string) error
	List() string
}

// ProcessTool implements the process management tool for background commands.
type ProcessTool struct {
	mgr     ProcessController
	cfg     config.ToolsConfig
	homeDir string
}

// NewProcessTool creates a process management tool.
func NewProcessTool(mgr ProcessController, cfg config.ToolsConfig, homeDir string) *ProcessTool {
	return &ProcessTool{mgr: mgr, cfg: cfg, homeDir: homeDir}
}

func (*ProcessTool) Name() string { return "process" }

func (*ProcessTool) Description() string {
	return `Manage background processes started by run_command with background=true.
- Use action "read" with id to read the output of a running background process.
- Use action "kill" with id to terminate a background process.
- Use action "list" to list all background processes and their status.`
}

func (*ProcessTool) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "Action to perform: \"read\" to get output, \"kill\" to terminate, \"list\" to list all processes.",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "Process ID returned by run_command with background=true.",
			},
		},
		"required": []string{"action"},
	}
}

func (p *ProcessTool) Execute(_ context.Context, params map[string]any) (string, error) {
	action, _ := params["action"].(string)
	switch action {
	case "read":
		id, _ := params["id"].(string)
		if id == "" {
			return "", fmt.Errorf("process: id is required for read")
		}
		output, err := p.mgr.Read(id)
		if err != nil {
			return "", err
		}
		return p.truncateOutput(output), nil
	case "kill":
		id, _ := params["id"].(string)
		if id == "" {
			return "", fmt.Errorf("process: id is required for kill")
		}
		if err := p.mgr.Kill(id); err != nil {
			return "", err
		}
		return fmt.Sprintf("Process %s terminated.", id), nil
	case "list":
		return p.mgr.List(), nil
	default:
		return "", fmt.Errorf("process: unknown action %q", action)
	}
}

func (p *ProcessTool) truncateOutput(output string) string {
	maxBytes := p.cfg.MaxOutputBytes
	if maxBytes <= 0 || len(output) <= maxBytes {
		return output
	}

	lines := strings.Split(output, "\n")
	totalLines := len(lines)
	if totalLines > 0 && lines[totalLines-1] == "" {
		lines = lines[:totalLines-1]
		totalLines--
	}

	if totalLines <= 20 {
		spillPath := p.spillFile()
		os.MkdirAll(filepath.Dir(spillPath), 0o700)
		os.WriteFile(spillPath, []byte(output), 0o600)
		result := p.perLineTruncate(output)
		return result + fmt.Sprintf("\n[Output truncated. Full output (%d bytes) saved to: %s]", len(output), spillPath)
	}

	firstLines := lines[:10]
	lastLines := lines[totalLines-10:]

	var buf bytes.Buffer
	for _, l := range firstLines {
		buf.WriteString(truncateLine(l, p.cfg.ReadLineMaxChars))
		buf.WriteByte('\n')
	}
	buf.WriteString(fmt.Sprintf("[Output truncated. Full output (%d bytes) saved to: %s]\n", len(output), p.spillFile()))
	for _, l := range lastLines {
		buf.WriteString(truncateLine(l, p.cfg.ReadLineMaxChars))
		buf.WriteByte('\n')
	}
	result := buf.String()
	if len(result) > 0 && result[len(result)-1] == '\n' {
		result = result[:len(result)-1]
	}
	return result
}

func (p *ProcessTool) spillFile() string {
	ts := time.Now().UnixNano()
	return filepath.Join(p.homeDir, ".lightcode", fmt.Sprintf("proc_output_%d_%x.txt", ts, ts%65536))
}

func (p *ProcessTool) perLineTruncate(s string) string {
	if p.cfg.ReadLineMaxChars <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	var buf bytes.Buffer
	for _, l := range lines {
		buf.WriteString(truncateLine(l, p.cfg.ReadLineMaxChars))
		buf.WriteByte('\n')
	}
	result := buf.String()
	if len(result) > 0 && result[len(result)-1] == '\n' {
		result = result[:len(result)-1]
	}
	return result
}
