package cli

import (
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/terminal"
)

func TestRenderBackgroundProcessUsesDedicatedHeaderAndOutputPreview(t *testing.T) {
	bg := &agent.BackgroundProcessDisplay{
		ID:       "bg-1",
		Command:  "printf 'one\ntwo'",
		Reason:   "completed",
		ExitCode: 0,
	}
	header := renderBackgroundProcessCall(bg, true)
	if !strings.Contains(header, "background_process") || !strings.Contains(header, "bg-1") || !strings.Contains(header, "completed exit 0") || !strings.Contains(header, "$ printf 'one") {
		t.Fatalf("background process header missing details: %q", header)
	}
	output := terminal.RenderToolResult("background_process", "", "1\n2\n3\n4\n5\n6", true, false, 80, nil)
	if strings.Contains(output, "6") || !strings.Contains(output, "(1 more line)") {
		t.Fatalf("background process output should use collapsed preview: %q", output)
	}
}

func TestPermissionPromptUsesSharedMenuRenderer(t *testing.T) {
	req := &agent.PermissionRequest{ToolName: "edit_file", Arg: "/tmp/file.txt"}

	rendered := renderPermissionPrompt(req, 0, 80)
	if got, want := strings.Count(rendered, nl)+1, permissionPromptRows(req); got != want {
		t.Fatalf("rendered rows = %d, permissionPromptRows = %d\n%s", got, want, rendered)
	}
	want := terminal.RenderMenu("Permission Required", permissionMenuItems(req), 2, 80, "")
	if rendered != want {
		t.Fatalf("permission prompt does not use shared menu renderer\nwant:\n%s\ngot:\n%s", want, rendered)
	}
	if strings.Contains(rendered, "┌") || strings.Contains(rendered, "│") || strings.Contains(rendered, "└") {
		t.Fatalf("permission prompt should use menu style, got boxed output:\n%s", rendered)
	}
}

func TestPermissionPromptShowsAffectedFilesWithoutStagingControls(t *testing.T) {
	req := &agent.PermissionRequest{
		ToolName:   "edit_file",
		Arg:        "/tmp/file.txt",
		BatchFiles: []string{"/tmp/file.txt", "/tmp/other.txt"},
	}

	rendered := renderPermissionPrompt(req, 0, 80)
	if !strings.Contains(rendered, "Affected files:") {
		t.Fatalf("permission prompt missing affected files label:\n%s", rendered)
	}
	if !strings.Contains(rendered, "/tmp/other.txt") {
		t.Fatalf("permission prompt missing affected file:\n%s", rendered)
	}
	if strings.Contains(rendered, "Staged files:") || strings.Contains(rendered, "Allow all") || strings.Contains(rendered, "(1/3)") {
		t.Fatalf("permission prompt must not expose staging-only controls:\n%s", rendered)
	}
	if got, want := strings.Count(rendered, nl)+1, permissionPromptRows(req); got != want {
		t.Fatalf("rendered rows = %d, permissionPromptRows = %d\n%s", got, want, rendered)
	}
	actions := permissionActions(req)
	if got := actions[0].Label; got != "Allow" {
		t.Fatalf("first permission action = %q, want Allow", got)
	}
	if got := actions[1].Label; got != "Deny" {
		t.Fatalf("second permission action = %q, want Deny", got)
	}
}

func TestPermissionPromptShowsResolvedTarget(t *testing.T) {
	req := &agent.PermissionRequest{
		ToolName:    "read_file",
		Arg:         "/tmp/project/notes.txt",
		ResolvedArg: "/tmp/project/.env",
	}

	rendered := renderPermissionPrompt(req, 0, 80)
	if !strings.Contains(rendered, "/tmp/project/notes.txt") {
		t.Fatalf("permission prompt missing requested path:\n%s", rendered)
	}
	if !strings.Contains(rendered, "resolves to /tmp/project/.env") {
		t.Fatalf("permission prompt missing resolved path:\n%s", rendered)
	}
	if got, want := strings.Count(rendered, nl)+1, permissionPromptRows(req); got != want {
		t.Fatalf("rendered rows = %d, permissionPromptRows = %d\n%s", got, want, rendered)
	}
}

func TestPermissionPromptHidesProjectSaveWhenDisabled(t *testing.T) {
	req := &agent.PermissionRequest{
		ToolName:           "apply_patch",
		Arg:                "/tmp/project/a.txt",
		BatchFiles:         []string{"/tmp/project/a.txt", "/tmp/project/.env"},
		BatchResolvedFiles: []string{"/tmp/project/a.txt", "/tmp/project/.env"},
		DisableProjectSave: true,
	}

	rendered := renderPermissionPrompt(req, 0, 80)
	if strings.Contains(rendered, "Allow for project") {
		t.Fatalf("permission prompt shows disabled project-save action:\n%s", rendered)
	}
	for _, action := range permissionActions(req) {
		if action.Extra == "project" {
			t.Fatalf("permission actions include disabled project-save: %#v", permissionActions(req))
		}
	}
}
