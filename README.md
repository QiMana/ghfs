ghfs [![godoc](https://godoc.org/github.com/benbjohnson/ghfs?status.png)](https://godoc.org/github.com/benbjohnson/ghfs) ![Version](http://img.shields.io/badge/status-alpha-red.png)
====

The GitHub Filesystem (GHFS) is a user space filesystem that overlays the
GitHub API. It allows you to access repositories and files using standard
Unix commands such as `ls` and `cat`.

This fork (`QiMana/ghfs`) includes modern build metadata, nested traversal fixes,
and operational commands for reliable agent workflows.

## Install

Build from source:

```sh
go build -o ghfs ./cmd/ghfs
```

## Commands

### Doctor

Validate local prerequisites and auth posture:

```sh
ghfs doctor
```

`doctor` exits non-zero only for hard blockers (e.g. missing FUSE).

### Mount

```sh
ghfs mount [--token <token>] [--token-file <path>] [--token-source env|none] <mountpoint>
```

For backward compatibility, legacy mode is still supported:

```sh
ghfs -token <token> <mountpoint>
# or
ghfs <mountpoint>
```

### Status

```sh
ghfs status <mountpoint>
```

Reports mount health, PID/state metadata, and exits non-zero when not mounted.

### Unmount

```sh
ghfs unmount <mountpoint>
```

Idempotent: if already unmounted, it reports that state and exits success.

## Path model

GHFS uses GitHub URL conventions for pathing:

```sh
/mount/<owner>
/mount/<owner>/<repo>
```

Once in a repository path, use normal Unix tools (`ls`, `cat`, etc.).
