package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DotEnvPath returns the path where Lightcode expects its .env file.
func DotEnvPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lightcode", ".env"), nil
}

// dotEnvTemplate is written to ~/.lightcode/.env the first time Lightcode
// runs. Empty values — the user uncomments a line and pastes the raw key
// after the `=`. No placeholder prefixes, no explanatory comments.
const dotEnvTemplate = `#OPENAI_API_KEY=
#OPENROUTER_API_KEY=
#MINIMAX_API_KEY=
#ZAI_API_KEY=
`

// LoadDotEnv reads ~/.lightcode/.env and populates the process environment
// with KEY=value entries that are not already set. Existing process env
// vars always win — a shell `export` takes precedence.
//
// On first run the file does not exist; LoadDotEnv creates the parent
// directory (mode 0700) and writes a commented template (mode 0600), then
// proceeds. The user only ever has to edit that one file.
//
// File format:
//
//	# full-line comments are allowed
//	KEY=value
//	KEY="quoted value"
//	export KEY=value   # shell-compat, "export " prefix is stripped
//
// Blank lines and full-line comments (#) are skipped. Malformed lines
// print a warning to stderr and are ignored.
func LoadDotEnv() error {
	path, err := DotEnvPath()
	if err != nil {
		return nil
	}

	// Auto-create the file with a template if it does not exist.
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: could not create %s: %v\n", filepath.Dir(path), err)
			return nil
		}
		if err := os.WriteFile(path, []byte(dotEnvTemplate), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: could not create %s: %v\n", path, err)
			return nil
		}
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			fmt.Fprintf(os.Stderr, "lightcode: .env %s:%d: skipping malformed line\n", path, lineNum)
			continue
		}

		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])

		// Strip matching surrounding quotes.
		if len(value) >= 2 {
			first := value[0]
			last := value[len(value)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		if _, alreadySet := os.LookupEnv(key); alreadySet {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: .env: setenv %s failed: %v\n", key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}
