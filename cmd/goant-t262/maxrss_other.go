//go:build !unix

package main

import "os/exec"

// childMaxRSS has no portable counterpart on Windows; the peak-memory report is
// a diagnostic, so it degrades to silence rather than to a wrong number.
func childMaxRSS(cmd *exec.Cmd) int64 { return 0 }
