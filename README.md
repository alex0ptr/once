# once

Run a command once, reuse its output for a while.

`once` executes a command in your current directory, prints its stdout and
keeps it in memory in a small per-user background daemon. Repeated calls with
the same command, directory and tenant key are answered from memory until the
entry expires. The daemon stops by itself once its last entry has expired.

The typical use case: reading secrets from 1Password without approving every
single read with your fingerprint.

```sh
export ONCE_TENANT="${ONCE_TENANT:-$(uuidgen)}"   # e.g. in ~/.zshrc

# later while you work in your scripts / direnv / mise.toml
export GITHUB_TOKEN="$(once --ttl 8h --no-dir -- op read op://Private/GitHub/token)"
```

The first call asks for your fingerprint, every further call within 8 hours
with the same tenant key does not.

## Install

```sh
go install github.com/alex0ptr/once@latest   # Go 1.27+, no dependencies besides the standard library
```

macOS and Linux only.

## Usage

```sh
once --help
```

This is the place to start: it lists every option and command
(`once help` and `-h` work too).
For more verbose documentation keep reading.

```
once --ttl DURATION [--until TIME] [--tenant KEY] [--refresh] [--no-dir] -- COMMAND [ARGS...]
once status
once clear
once help
```

| Option | Meaning |
|---|---|
| `--ttl` | How long to cache the result, e.g. `30m`, `1h`, `24h` (required). |
| `--until` | Absolute upper bound for the expiry (see below). |
| `--tenant` | Key that separates cache contexts (see below). Defaults to `$ONCE_TENANT`; one of both is required. |
| `--refresh` | Ignore the cache, run the command and overwrite the cached value. |
| `--no-dir` | Leave the working directory out of the cache key (see below). |

`once status` shows the daemon, its entry count and when it will stop.
`once clear` drops every cached value and stops the daemon.

Everything after `--` is executed directly (no shell). Environment variables,
working directory and terminal are inherited from your shell. If you need
pipes, globs or other shell features, wrap them yourself:

```sh
once --ttl 1h -- sh -c 'op item get "AWS" --format json | jq -r .fields[0].value'
```

Only stdout is cached. stdin and stderr are passed through, so interactive
prompts keep working. Results of commands that exit non-zero are never cached,
and the exit code is passed on.

## Directory-independent caching with `--no-dir`

By default the working directory is part of the cache key, because the same
command can produce different output in different directories (`git rev-parse
HEAD`, `cat .version`, …). For commands whose output does not depend on the
directory, such as `op read`, pass `--no-dir` so one cached value serves every
directory. On a miss the command still runs in your current directory.

Entries created with and without `--no-dir` are separate.

## Limiting the cache with `--until`

`--until` takes an absolute time; the entry expires at whichever comes first,
`--ttl` or `--until`. `once` itself never computes relative dates; let your
shell do that.

Accepted formats: `2026-10-09T06:00:00+02:00`, `2026-10-09T06:00:00+0200`,
`2026-10-09T06:00`, `2026-10-09` (forms without a zone are local time).

Cache for up to a day, but never past 10 pm today:

```sh
once --ttl 24h --until "$(date +%F)T22:00" --no-dir -- op read op://Work/DB/password
```

Never past tomorrow morning:

```sh
# GNU date (Linux)
once --ttl 24h --until "$(date -d tomorrow +%F)T06:00" -- op read …
# BSD date (macOS)
once --ttl 24h --until "$(date -v+1d +%F)T06:00" -- op read …
```

If `--until` already lies in the past, the command runs normally and its
result is not cached.

## Tenant keys

The cache key is `HMAC-SHA256(tenant, working directory ‖ command ‖ args)`
(without the directory when `--no-dir` is set).

The tenant key is a namespace, not a security boundary, and it is not meant
to be kept secret; it may well live in a config file. It does two things:

- It separates cache contexts (shells, projects, scripts): a context with a
  different tenant key gets its own entries and has to fetch its secrets
  itself.
- It makes cache keys practically unguessable.

It does not stop other processes of your own user from using the cache; see
[Security model](#security-model).

The `${ONCE_TENANT:-$(uuidgen)}` pattern above gives every new terminal its
own tenant while subshells and scripts started from it share the cache.
Use a fixed value to share the cache across terminals.

(Side note: a key passed via `--tenant` shows up in the process list.)

## How it works

1. `once` computes the key and asks the daemon over a Unix socket.
2. On a hit, it prints the cached output and exits.
3. On a miss, it runs the command itself, in your directory, with your
   environment. If the command succeeds, `once` starts the daemon if needed
   and hands it the output together with the expiry time.
4. The daemon keeps entries in memory only. It tracks the latest expiry and
   shuts down when no entry is left.

The socket lives in `$XDG_RUNTIME_DIR/once-<uid>/` (or the temp dir) in a
directory with mode `0700`, so other users cannot connect. That directory also
holds `daemon.log`. Set `ONCE_RUNTIME_DIR` to use a different location.

The client computes the cache key itself, so the tenant key is never sent to
the daemon.

## Security model

- Protection against other users comes from the Unix socket and file
  permissions: the runtime directory has mode `0700`, the socket `0600`.
- Processes of the same user are not kept out. They can talk to the socket
  and can read the tenant key (from the environment or a config file), so
  they can use the cache just like you. This is accepted by design.
- Values are kept unencrypted in the daemon's memory. They are overwritten
  before they are dropped (best effort; memory is not locked against
  swapping).
- Core dumps of the daemon are disabled (`RLIMIT_CORE` 0, plus
  `PR_SET_DUMPABLE` 0 on Linux), so cached values do not end up in a core
  file.

## Limits

- Outputs larger than 64 MiB are passed through but not cached.
- Expiry is checked against the wall clock on every read, so entries also
  expire correctly after a laptop has been asleep.
