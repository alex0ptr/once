package main

import (
	"log"
	"syscall"
)

// disableCoreDumps keeps cached values out of core files. On Linux the
// process is additionally marked non-dumpable, which also blocks ptrace
// attaches by non-root processes of the same user.
func disableCoreDumps() {
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		log.Printf("disable core dumps: setrlimit: %v", err)
	}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		log.Printf("disable core dumps: prctl: %v", errno)
	}
}
