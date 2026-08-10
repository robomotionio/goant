//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// childMaxRSS is the peak resident size of a finished child, in bytes.
//
// Linux reports ru_maxrss in kilobytes, macOS in bytes. The difference is not
// documented anywhere near getrusage and is a classic way to be off by 1024.
func childMaxRSS(cmd *exec.Cmd) int64 {
	if cmd.ProcessState == nil {
		return 0
	}
	ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0
	}
	return ru.Maxrss * maxRSSUnit
}
