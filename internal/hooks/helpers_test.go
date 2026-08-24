//go:build linux

package hooks

import (
	"os/exec"
	"strconv"
	"strings"
)

func countProcs(pattern string) int {
	out, err := exec.Command("pgrep", "-fc", pattern).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func exec_kill(pattern string) { _ = exec.Command("pkill", "-f", pattern).Run() }
