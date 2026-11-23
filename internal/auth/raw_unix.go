//go:build !windows

package auth

import (
	"bufio"
	"os"
	"strings"
)

func readSingleKey() (string, error) {
	rd := bufio.NewReader(os.Stdin)
	line, err := rd.ReadString('\n')
	if err != nil {
		return "", err
	}
	s := strings.ToLower(strings.TrimSpace(line))
	if s == "" {
		return "enter", nil
	}
	return s[:1], nil
}
