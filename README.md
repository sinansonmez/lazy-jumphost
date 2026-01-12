# lazy-jumphost

Terminal UI for managing Cloud SQL SSH tunnels through a jumphost.

## Requirements

- Go 1.21+
- `ssh` available in your PATH
- Access to the jumphost and `cloudsql_access.sh`

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
```

Use the Start/Stop buttons to manage tunnels and Refresh to update status.

Use `-debug` to show a debug log panel inside the UI.
