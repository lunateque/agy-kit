//go:build !windows

package auth

import (
	"os"
	"strings"
)

func readAutoCompactOverride() string {
	return strings.TrimSpace(os.Getenv("AGY_CLI_AUTO_COMPACT_OVERRIDE"))
}
