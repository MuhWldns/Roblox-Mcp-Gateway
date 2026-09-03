package mcpprocess

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

// Command is a fully resolved local executable and its fixed arguments.
type Command struct {
	Path string
	Args []string
}

// Launcher resolves one trusted local command configured by the bridge owner.
// It deliberately has no method that accepts values from a remote request.
type Launcher struct {
	trustedPath string
	trustedArgs []string
}

func NewLauncher(trustedPath string, trustedArgs ...string) Launcher {
	return Launcher{
		trustedPath: trustedPath,
		trustedArgs: append([]string(nil), trustedArgs...),
	}
}

func (l Launcher) Resolve() (Command, error) {
	if err := validateTrustedValue("launcher path", l.trustedPath); err != nil {
		return Command{}, err
	}
	if strings.Contains(l.trustedPath, "://") {
		return Command{}, errors.New("launcher path must be local")
	}
	if runtime.GOOS == "windows" && strings.HasPrefix(filepath.Clean(l.trustedPath), `\\`) {
		return Command{}, errors.New("launcher path must not be a network share")
	}

	trustedPath, err := canonicalFile(l.trustedPath)
	if err != nil {
		return Command{}, fmt.Errorf("resolve trusted launcher: %w", err)
	}
	for _, arg := range l.trustedArgs {
		if err := validateTrustedValue("launcher argument", arg); err != nil {
			return Command{}, err
		}
	}

	if runtime.GOOS == "windows" && strings.EqualFold(filepath.Ext(trustedPath), ".bat") {
		if len(l.trustedArgs) != 0 {
			return Command{}, errors.New("Windows batch launcher does not accept arguments")
		}
		comspec := os.Getenv("COMSPEC")
		if err := validateTrustedValue("COMSPEC", comspec); err != nil {
			return Command{}, err
		}
		comspec, err = canonicalFile(comspec)
		if err != nil {
			return Command{}, fmt.Errorf("resolve COMSPEC: %w", err)
		}
		return Command{Path: comspec, Args: []string{"/d", "/s", "/c", quoteBatchPath(trustedPath)}}, nil
	}

	return Command{Path: trustedPath, Args: append([]string(nil), l.trustedArgs...)}, nil
}

func quoteBatchPath(path string) string {
	return `""` + strings.ReplaceAll(path, `"`, `""`) + `""`
}

func canonicalFile(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("path is not a regular file")
	}
	return canonical, nil
}

func validateTrustedValue(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", name)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains a control character", name)
	}
	return nil
}
