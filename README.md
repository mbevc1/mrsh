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

`mrsh hosts example` prints an annotated example config ([source](internal/config/example.yaml)).

## Commands

| Command | What it does |
|---------|--------------|
| `run -c CMD` / `run --script FILE` (aliases `ru`, `r`) | Run a command, or a local script piped to `sh -s`. With neither, runs the config's `commands:` list. |
| `credentials list\|set\|remove\|rename\|default\|assign` (aliases `creds`, `cred`) | Manage reusable credential sets (user, pass, identity file): `default NAME` sets the one `defaults` uses, `assign --name NAME -g GROUP` (or `-H HOST`, or `--clear`) points many hosts at one, `rename` updates every reference. See [Secrets](#secrets). |
| `hosts init\|example\|list\|add\|update\|remove\|copy` (aliases `host`, `ho`, `h`) | Manage the host inventory. `example` prints an annotated config. `list` masks literal passwords unless `--show-secrets`. `copy --to DEST` moves the config between local paths and S3 (see [Config and output](#config-and-output)). |
| `mt backup` (aliases `b`, `ba`, `bck`) | Export `.rsc` and binary `.backup` files into `backups/` on each router (a rolling 7-day set, e.g. `backups/Mon.rsc`), then download them to `--path`: a local dir keeps the same weekday names (`<group>/<name>/Mon.rsc`), and `s3://bucket/prefix/` keeps dated copies (`<group>/<name>/2026-04-06.rsc`; expire them with a lifecycle rule). Files from older mrsh versions (`backup-<Day>.*` in the device root) are left in place. |
| `mt reboot` (alias `re`), `mt upgrade` (alias `up`) | Reboot, or take the next upgrade step (RouterOS packages, then routerboard firmware); each step ends in a reboot, so a device behind on both needs two runs. `mt upgrade --wait` instead waits for each reboot (up to 10 minutes per device) and takes every step in one run, ending with a fresh check. Both commands ask for confirmation unless `-y` / `--assume-yes`. |
| `mt version` (aliases `v`, `ver`) | Firmware and package versions across the fleet. |
| `ui` | A local browser editor for the same config (loopback only): hosts, credential sets, defaults and commands. Its **Test** button and tab connect to saved hosts and run a read-only version check (RouterOS version and board, or "connected" for other hosts). Its **Upgrade** tab runs the `mt upgrade` step on ticked devices after a confirmation (devices reboot), with a dry run option and a **Finish all steps** box that works like `--wait`. It runs no other commands. |
| `completion bash\|zsh\|fish\|powershell` | Shell completion; host names, groups and credential set names complete from the config. |
| `version` (aliases `v`, `ver`), or `-v` / `--version` | Build info. |

Subcommands have short aliases too: `init` `i`, `example` `ex`, `copy` `cp`/`co`, `list` `l`/`ls`, `add` `a`, `update` `u`/`up`, `remove` `r`/`rm`, `set` `s`, `assign` `ass`. So `mrsh h ls` is `mrsh hosts list` and `mrsh creds rm` is `mrsh credentials remove`.

## Targeting hosts

- `-g GROUP` selects a group; `-H NAME` (repeatable, comma-separated, alias `--hosts`) selects hosts by config name or address. Both together select the union.
- A `-H` value that matches no config entry is treated as a literal address, `[user@]host[:port]`. `--debug` logs each such fall-through, which catches typos.
- `--dry-run` prints the targets and the command without connecting.

## Secrets

For each host, in `defaults` and in each credential set, set at most one form of `user` and of `pass`:

| Key | Meaning |
|-----|---------|
| `user`, `pass` | Literal value, used verbatim |
| `user_env`, `pass_env` | Name of an environment variable |
| `user_arn`, `pass_arn` | AWS SSM parameter or Secrets Manager ARN; `#key` picks a field from a JSON secret |

`identity_file` (an SSH private key) can be set the same places, as a path.

Credential sets bundle a login (user, pass and optionally `identity_file`) for reuse: define them under `credentials:` and reference one with `credential: NAME` on a host or in `defaults` (or `mrsh hosts add/update --credential NAME`). For each of user, pass and identity file, a host uses its own value, else its set's, else `defaults`, else the set named in `defaults`, so a host can share a set and still override one field. `mrsh credentials` (aliases `creds`, `cred`) manages them, as does the UI's Credentials tab; a set in use cannot be removed:

```bash
mrsh creds set --name routers -u admin --pass-arn arn:aws:ssm:eu-west-1:123456789012:parameter/mrsh/routers
mrsh creds assign --name routers -g routers     # every host in the group
mrsh creds default routers                      # hosts with no set of their own
mrsh creds rename --name routers --to mikrotik  # updates every reference
mrsh creds list                                 # sets and who uses them
```

```yaml
credentials:
  routers: {user: admin, pass_arn: arn:aws:ssm:eu-west-1:123456789012:parameter/mrsh/routers}
hosts:
  - {name: rb01, host: 10.0.0.254, credential: routers}
  - {name: rb02, host: 10.0.0.253, credential: routers, pass_env: RB02_PASS}
```

Secrets are resolved only for the hosts a command targets, using the standard AWS credential chain. `--debug` logs which source each secret came from, never its value. A literal `pass` makes the config file itself a secret, so every command that loads such a config warns, naming where (`mrsh: warning: hosts.yaml stores 1 literal password(s) (host web02); …`), and so does `--pass` on `hosts add/update` and `credentials set`.

## Host keys

`--host-key-policy` (or `defaults.host_key_policy`) is `insecure` by default, and mrsh prints a warning on stderr each run until you set a policy explicitly (setting `insecure` yourself silences it). Use `accept-new` to record keys on first connect and reject any later change, or `strict` to require entries in `--known-hosts` (default `~/.ssh/known_hosts`).

## Config and output

- `-f` takes a local path or `s3://bucket/key`, and defaults to `$MRSH_CONFIG` when set, else `hosts.yaml`. Edits from the CLI and `mrsh ui` are guarded: a write fails, then reloads and reapplies, if the file changed after it was read (a content hash locally, the ETag on S3).
- To move the config, `mrsh -f hosts.yaml hosts copy --to s3://my-bucket/mrsh/hosts.yaml` validates it and copies it byte for byte (comments kept), in either direction. It never replaces an existing destination unless `--force`, honours `--sse`/`--kms-key`, warns if the config holds literal passwords, and leaves the source in place. Then `export MRSH_CONFIG=s3://my-bucket/mrsh/hosts.yaml` points every later command at it.
- `--sse AES256|aws:kms` and `--kms-key ARN` encrypt S3 writes (config and backups).
- Precedence: CLI flag > `MRSH_DEBUG` > `defaults` in the config > built-in default.
- `-o text|full|json|csv`. `text` is a table showing the first output line per host (a failed host shows `failed` under EXIT when there is no remote exit status, and its error ahead of any output); `full` prints each host's complete output under a header line. Logs go to stderr, so `mrsh -o json run -c uptime | jq` works with `--debug` on.
- Warnings and errors go to stderr only, never stdout. `-q`/`--quiet` silences warnings (errors still print) and overrides `MRSH_DEBUG`; it can't be combined with `--debug`.
- Exit codes: `0` all hosts ok, `1` any host failed or exited non-zero, `2` usage or config error (including a declined confirmation, or a host whose secret can't be resolved, before any host is contacted), `130` interrupted by Ctrl-C or SIGTERM (also at a confirmation prompt). Stopping `mrsh ui` with Ctrl-C or its Exit button exits `0`.

## Container image

Tagged releases and `main` publish a signed multi-arch image (`linux/amd64`, `linux/arm64`) to `ghcr.io/mbevc1/mrsh`, with an SBOM and provenance. It is distroless: no shell, UID 65532, and it works with a read-only root filesystem.

```bash
docker run --rm --read-only --cap-drop ALL \
  -v "$PWD/hosts.yaml:/home/nonroot/hosts.yaml:ro" \
  ghcr.io/mbevc1/mrsh -g web run -c uptime

# config and backups in S3; mrsh writes only to /tmp (known_hosts with accept-new)
docker run --rm --read-only --tmpfs /tmp -e HOME=/tmp -e AWS_REGION=eu-west-1 \
  ghcr.io/mbevc1/mrsh -f s3://my-bucket/mrsh/hosts.yaml -g routers mt backup --path s3://my-bucket/mrsh/backups/
```

`make docker` builds it locally. `mrsh ui` listens on loopback only, so it is not reachable from outside a container.

## Scheduled runs on AWS

`deploy/cloudformation.yaml` runs mrsh as an ECS Fargate task on an EventBridge Scheduler schedule. The task has no time limit and pulls the image from any registry, GHCR included. By default it gets a public IP in a public subnet, so it needs no NAT gateway; the subnet must still reach the managed hosts (VPN/TGW routes or their public IPs). With `AssignPublicIp=DISABLED`, use private subnets with NAT or VPC endpoints. The IAM policy covers only the config object and, when set, the backup prefix, SSM/Secrets Manager prefixes and one KMS key.

```bash
aws cloudformation deploy --stack-name mrsh-backup --capabilities CAPABILITY_IAM \
  --template-file deploy/cloudformation.yaml --parameter-overrides \
  ImageUri=ghcr.io/mbevc1/mrsh:0.1.0 \
  MrshArgs="-f,s3://my-bucket/mrsh/hosts.yaml,-g,routers,mt,backup,--path,s3://my-bucket/mrsh/backups/" \
  ConfigBucket=my-bucket ConfigKey=mrsh/hosts.yaml BackupBucket=my-bucket \
  CreateBackupBucket=true BackupRetentionDays=90 VersionRetentionDays=90 NotificationEmail=ops@example.com \
  SsmParameterPrefix=mrsh/ SubnetIds=subnet-aaa,subnet-bbb SecurityGroupIds=sg-ccc
```

`CreateBackupBucket=true` creates `BackupBucket` (private, SSE-S3, TLS only, versioned, kept when the stack is deleted) with a rule that expires backups after `BackupRetentionDays`. Upload the config after the stack exists and keep it in the same bucket, outside `BackupPrefix`: S3 is the recommended home for it (mrsh's conditional writes stop two editors overwriting each other), and versioning keeps every previous save for `VersionRetentionDays` (default 90), so a bad edit can be rolled back by restoring the previous version (`aws s3api list-object-versions`, then copy it back). Expired backups also linger as old versions for that long. Keep passwords out of the config itself: use `pass_arn` (SSM `SecureString`) or `pass_env`.

`NotificationEmail` turns on failure emails: a run that exits non-zero (1: a host failed, 2: usage or config error), a task that fails to start, or a schedule that cannot launch one. Confirm the subscription email from AWS once. Successful runs send nothing. Slack or Teams can subscribe to the same SNS topic (output `NotificationTopicArn`) through Amazon Q Developer in chat applications.

Cost:

- **Network:** the public IP default avoids a NAT gateway (about $32 a month), by far the largest cost for a daily task. Compute is a few cents a month.
- **Secrets:** SSM `SecureString` parameters are free; Secrets Manager costs $0.40 per secret per month.
- **Encryption:** SSE-S3 and the `aws/ssm` key are free; a customer KMS key (`KmsKeyArn`) costs $1 a month plus requests.
- **Storage:** backups are small, so keep them in S3 Standard; Standard-IA and Glacier bill each object as at least 128 KB.

Things to know:

- **Paths:** use `s3://` for the config and backups; the container filesystem is read-only apart from `/tmp`.
- **Prompts:** there is no terminal, so `mt reboot`, `mt upgrade` and `hosts remove` need `-y` / `--assume-yes`.
- **No replays:** retries are off, so a failed reboot or upgrade is not repeated.
- **Host keys:** `/tmp` does not survive between runs, so `accept-new` cannot remember hosts. For verified host keys, use `strict` with a `known_hosts` file baked into a derived image.

## Development

```bash
make build      # ./mrsh with version info from git
make test       # race detector + coverage
make lint       # golangci-lint
make snapshot   # goreleaser cross-compile into ./dist (needs goreleaser)
make clean      # remove binaries, dist/, coverage and test output
make help       # list all targets
```

Tests need no network or cloud access: SSH, SFTP and S3 run against in-process fakes. Releases are cut by pushing a `v*` tag.

## License

[MPL-2.0](LICENSE)
