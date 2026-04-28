# lazy-jumphost

Terminal UI for managing Cloud SQL SSH tunnels through a jumphost.

## Requirements

- Go 1.21+ (only needed to run from the source code)
- `ssh` available in your PATH (optional)
- Access to the jumphost and `cloudsql_access.sh`
- Git Bash is supported on Windows.

## Configuration

Copy the sample configuration and edit as needed:

```bash
cp config.example.yaml config.yaml
```

Each connection requires:

- `name`: Friendly label shown in the UI.
- `user`: SSH user on the jumphost.
- `host`: Jumphost address.
- `cloudsql_instance`: Cloud SQL instance identifier.
- `local_port`: Local port to bind.
- `socket_path` (optional): Override the remote socket path if it differs from
  `/home/<user>/<cloudsql_instance>/.s.PGSQL.5432`.

## Run

```bash
go run . -config config.yaml
./dist/darwin/arm64/lazy-jumphost -config config.yaml -debug
```

Use the Start/Stop buttons to manage tunnels and Refresh to update status.

Use `-debug` to show a debug log panel inside the UI. Debug logs are also
written to `lazy-jumphost-debug.txt` by default:

```bash
lazy-jumphost -config config.yaml -debug
lazy-jumphost -config config.yaml -debug -log-file windows-run.txt
lazy-jumphost -config config.yaml -debug -log-file ""
```

The last command disables file logging. ANSI color codes from remote commands
are stripped in the debug output.

Password prompts appear once per connection start and are reused for the
Cloud SQL access step and the SSH tunnel. If the password is incorrect, you'll
be prompted again when the remote asks.

Before starting a connection, the app checks whether `127.0.0.1:<local_port>`
is available. If the port is already in use, start fails before running the
Cloud SQL access step.

The Cloud SQL access step is considered ready when `cloudsql_access.sh` prints
the local `ssh -fnNT -L ...` tunnel command. The app then starts and manages its
own foreground SSH tunnel so Stop can terminate it cleanly.
