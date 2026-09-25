# mrsh

Multi Remote SHell: run commands over SSH on many hosts in parallel, with MikroTik RouterOS shortcuts, secrets kept out of the config file, and config and backups stored locally or in S3.

## Install

Download an archive from [Releases](https://github.com/mbevc1/mrsh/releases), or:

```bash
go install github.com/mbevc1/mrsh@latest
```

## Quick start

```bash
mrsh hosts init                                   # writes a starter hosts.yaml
mrsh hosts add --name web01 -H 10.0.0.1 -g web --pass-env WEB01_PASS
mrsh -g web -p 5 run -c uptime                    # run on the web group, 5 at a time
mrsh -H admin@10.0.0.9:2222 run -c 'df -h'        # a host not in the config
```

`hosts.example.yaml` shows every config option.

## Commands

| Command | What it does |
|---------|--------------|
| `run -c CMD` / `run --script FILE` | Run a command, or a local script piped to `sh -s`. With neither, runs the config's `commands:` list. |
| `hosts init\|list\|add\|update\|remove` | Manage the host inventory. `list` masks literal secrets unless `--show-secrets`. |
| `mt backup` | Export `.rsc` and binary `.backup` files on each router (kept on the device as a rolling 7-day set), then download dated copies to `--path` (a local dir or `s3://bucket/prefix/`). |
| `mt reboot`, `mt upgrade` | Reboot, or take the next upgrade step (RouterOS packages, then routerboard firmware). Both ask for confirmation unless `--confirm`. |
| `mt version` | Firmware and package versions across the fleet. |
| `ui` | A local browser editor for the same config (loopback only; edits config, never runs commands). |
| `completion bash\|zsh\|fish\|powershell` | Shell completion; host names and groups complete from the config. |
| `version` | Build info. |

## Targeting hosts

- `-g GROUP` selects a group; `-H NAME` (repeatable, comma-separated, alias `--hosts`) selects hosts by config name or address. Both together select the union.
- A `-H` value that matches no config entry is treated as a literal address, `[user@]host[:port]`. `--debug` logs each such fall-through, which catches typos.
- `--dry-run` prints the targets and the command without connecting.

## Secrets

For each host (and in `defaults`), set at most one form of `user` and of `pass`:

| Key | Meaning |
|-----|---------|
| `pass` | Literal value, used verbatim |
| `pass_env` | Name of an environment variable |
| `pass_arn` | AWS SSM parameter or Secrets Manager ARN; `#key` picks a field from a JSON secret |

Secrets are resolved only for the hosts a command targets, using the standard AWS credential chain. `--debug` logs which source each secret came from, never its value.

## Host keys

`--host-key-policy` (or `defaults.host_key_policy`) is `insecure` by default, and mrsh prints a warning on each run. Use `accept-new` to record keys on first connect and reject any later change, or `strict` to require entries in `--known-hosts` (default `~/.ssh/known_hosts`).

## Config and output

- `-f` takes a local path or `s3://bucket/key`. Edits from the CLI and `mrsh ui` are guarded: a write fails, then reloads and reapplies, if the file changed after it was read (a file lock locally, the ETag on S3).
- `--sse AES256|aws:kms` and `--kms-key ARN` encrypt S3 writes (config and backups).
- Precedence: CLI flag > `MRSH_DEBUG` > `defaults` in the config > built-in default.
- `-o text|json|csv`. Logs go to stderr, so `mrsh -o json run -c uptime | jq` works with `--debug` on.
- Exit codes: `0` all hosts ok, `1` any host failed or exited non-zero, `2` usage or config error, `130` interrupted.

## Development

```bash
make build      # ./bin/mrsh with version info from git
make test       # race detector + coverage
make lint       # golangci-lint
make snapshot   # goreleaser cross-compile into ./dist (needs goreleaser)
```

Tests need no network or cloud access: SSH, SFTP and S3 run against in-process fakes. Releases are cut by pushing a `v*` tag.

## License

[MPL-2.0](LICENSE)
