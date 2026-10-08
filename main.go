// Command once runs a command and caches its stdout in a per-user background
// daemon, so repeated invocations (e.g. reading a secret from 1Password) are
// served from memory until the cache entry expires.
package main

import (
	"fmt"
	"os"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "__daemon":
			return runDaemon()
		case "status":
			return runStatus()
		case "clear":
			return runClear()
		case "help", "-h", "-help", "--help":
			usage()
			return 0
		}
	}
	return runCached(args)
}

func usage() {
	_, _ = fmt.Fprint(os.Stderr, `Usage:
  once --ttl DURATION [--until TIME] [--tenant KEY] [--refresh] [--no-dir] -- COMMAND [ARGS...]
  once status
  once clear
  once --help

Runs COMMAND in the current directory and caches its stdout. Later calls with
the same command, directory and tenant key are answered from the cache.

Options:
  --ttl DURATION   how long to cache the result, e.g. 30m, 1h, 24h (required)
  --until TIME     absolute upper bound for the expiry, e.g. 2026-10-09T06:00,
                   2026-10-09T06:00:00+02:00 or 2026-10-09 (local time if no zone)
  --tenant KEY     key that separates cache contexts (default: $ONCE_TENANT,
                   one is required)
  --refresh        ignore any cached value, run COMMAND and overwrite the cache
  --no-dir         share the cached value across all directories (the command
                   still runs in the current directory on a miss)

Commands:
  status           show the state of the background daemon
  clear            drop all cached values and stop the daemon
  help, --help     show this help

Results of commands exiting non-zero are never cached.
`)
}

func warnf(format string, a ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "once: "+format+"\n", a...)
}

// usageErrorf reports a wrong invocation and points to --help.
func usageErrorf(format string, a ...any) int {
	warnf(format, a...)
	_, _ = fmt.Fprintln(os.Stderr, "Run 'once --help' for usage.")
	return 2
}
