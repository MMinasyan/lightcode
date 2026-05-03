package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

func writeClipboard(w io.Writer, text string) error {
	b64 := base64.StdEncoding.EncodeToString([]byte(text))
	_, err := fmt.Fprintf(w, "\033]52;c;%s\a", b64)
	if err != nil {
		return writeClipboardFallback(text)
	}
	return nil
}

func writeClipboardFallback(text string) error {
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
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	return fmt.Errorf("no clipboard tool found (install xclip, xsel, or wl-copy)")
}
