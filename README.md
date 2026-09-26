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
| `run -c CMD` / `run --script FILE` (aliases `ru`, `r`) | Run a command, or a local script piped to `sh -s`. With neither, runs the config's `commands:` list. |
| `hosts init\|list\|add\|update\|remove` (aliases `ho`, `h`) | Manage the host inventory. `list` masks literal passwords unless `--show-secrets`. |
| `mt backup` | Export `.rsc` and binary `.backup` files on each router (kept on the device as a rolling 7-day set), then download dated copies to `--path` (a local dir or `s3://bucket/prefix/`). |
| `mt reboot`, `mt upgrade` | Reboot, or take the next upgrade step (RouterOS packages, then routerboard firmware). Both ask for confirmation unless `--confirm`. |
| `mt version` | Firmware and package versions across the fleet. |
| `ui` | A local browser editor for the same config (loopback only; edits config, never runs commands). |
| `completion bash\|zsh\|fish\|powershell` | Shell completion; host names and groups complete from the config. |
| `version` (aliases `v`, `ver`), or `-v` / `--version` | Build info. |

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

- `-f` takes a local path or `s3://bucket/key`. Edits from the CLI and `mrsh ui` are guarded: a write fails, then reloads and reapplies, if the file changed after it was read (a content hash locally, the ETag on S3).
- `--sse AES256|aws:kms` and `--kms-key ARN` encrypt S3 writes (config and backups).
- Precedence: CLI flag > `MRSH_DEBUG` > `defaults` in the config > built-in default.
- `-o text|json|csv`. Logs go to stderr, so `mrsh -o json run -c uptime | jq` works with `--debug` on.
- Exit codes: `0` all hosts ok, `1` any host failed or exited non-zero, `2` usage or config error, `130` interrupted.

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

`make docker` builds it locally. Behind a TLS-inspecting proxy, pass its CA bundle: `make docker DOCKER_CA=/path/to/ca.crt`. `mrsh ui` listens on loopback only, so it is not reachable from outside a container.

## Scheduled runs on AWS

`deploy/cloudformation.yaml` runs mrsh on an EventBridge Scheduler schedule, either as an **ECS Fargate task** (default) or as a **Lambda function**. Both need subnets that reach the managed hosts and the AWS APIs (NAT or VPC endpoints). The IAM policy covers only the config object and, when set, the backup prefix, SSM/Secrets Manager prefixes and one KMS key.

| | ECS task (default) | Lambda |
|---|---|---|
| Run time | unlimited | 15 minutes |
| Image registry | any (e.g. GHCR) | private ECR, built with `--provenance=false` |
| Output | CloudWatch Logs | CloudWatch Logs, plus a 256 KiB result |
| Best for | backups, upgrades, large fleets | quick checks on a few hosts |

```bash
# ECS
aws cloudformation deploy --stack-name mrsh-backup --capabilities CAPABILITY_IAM \
  --template-file deploy/cloudformation.yaml --parameter-overrides \
  ImageUri=ghcr.io/mbevc1/mrsh:0.1.0 \
  MrshArgs="-f,s3://my-bucket/mrsh/hosts.yaml,-g,routers,mt,backup,--path,s3://my-bucket/mrsh/backups/" \
  ConfigBucket=my-bucket ConfigKey=mrsh/hosts.yaml BackupBucket=my-bucket \
  SsmParameterPrefix=mrsh/ SubnetIds=subnet-aaa,subnet-bbb SecurityGroupIds=sg-ccc

# Lambda: push an ECR image without attestations first
docker buildx build --platform linux/arm64 --provenance=false --sbom=false \
  -t 123456789012.dkr.ecr.eu-west-1.amazonaws.com/mrsh:0.1.0 --push .
aws cloudformation deploy ... --parameter-overrides DeployTarget=Lambda \
  ImageUri=123456789012.dkr.ecr.eu-west-1.amazonaws.com/mrsh:0.1.0 ...
```

Things to know:

- **Paths:** use `s3://` for the config and backups; the container filesystem is read-only apart from `/tmp`.
- **Prompts:** there is no terminal, so `mt reboot`, `mt upgrade` and `hosts remove` need `--confirm`.
- **No replays:** retries are off, so a failed reboot or upgrade is not repeated.
- **Host keys:** `/tmp` does not survive between runs, so `accept-new` cannot remember hosts. For verified host keys, use `strict` with a `known_hosts` file baked into a derived image.
- **Lambda events:** the function takes `{"args": [...]}`. `aws lambda invoke --function-name <FunctionArn> --cli-binary-format raw-in-base64-out --payload '{"args":["mt","version"]}' out.json` runs a one-off.

## Development

```bash
make build      # ./mrsh with version info from git
make test       # race detector + coverage
make lint       # golangci-lint
make snapshot   # goreleaser cross-compile into ./dist (needs goreleaser)
```

Tests need no network or cloud access: SSH, SFTP and S3 run against in-process fakes. Releases are cut by pushing a `v*` tag.

## License

[MPL-2.0](LICENSE)
