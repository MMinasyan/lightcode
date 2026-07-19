package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// writeClipboardOSC52 emits the OSC-52 clipboard sequence to w. It is ordinary
// mainLoop output; the caller runs the fallback (in the op-group) only if this
// returns a write error.
func writeClipboardOSC52(w io.Writer, text string) error {
	b64 := base64.StdEncoding.EncodeToString([]byte(text))
	_, err := fmt.Fprintf(w, "\033]52;c;%s\a", b64)
	return err
}

// writeClipboardFallback shells out to an installed clipboard tool. It is
// context-cancellable so host shutdown can kill a blocked tool instead of wedging
// the op-group.
func writeClipboardFallback(ctx context.Context, text string) error {
	tools := [][]string{
		{"xsel", "--clipboard", "--input"},
		{"xclip", "-selection", "clipboard"},
		{"wl-copy"},
		{"pbcopy"},
		{"clip.exe"},
	}
	for _, args := range tools {
		if _, err := exec.LookPath(args[0]); err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	return fmt.Errorf("no clipboard tool found (install xclip, xsel, or wl-copy)")
}
