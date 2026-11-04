//go:build unix

package main

import "syscall"

// setLowPriority lowers a child process's CPU priority (nice 10) so analysis
// never competes with recording, or with radiod on the same host.
func setLowPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 10)
}
