# Cloudflare Tunnel Manager

Status: early project. Token permissions have been checked against a real Cloudflare account; creating tunnels and hosts has only been tested against a simulated API, and the Docker image has not been built yet.

A self-hosted panel, in the style of Nginx Proxy Manager, for publishing services on your network through
**Cloudflare Tunnels**: you add a host (`app.yourdomain.com` → `http://192.168.1.10:8080`) and the panel creates the
route in the tunnel and the DNS record. Because the tunnel is an outbound connection, **you do not need to open any ports on your router**.

- Hosts with automatic DNS, enable/disable, path-based routes and wildcards (`*.yourdomain.com`).
- Multiple tunnels: the connector can run on this server (the panel starts and supervises `cloudflared`) or on another machine.
- Cloudflare Access per host: sign-in for the emails or domains you choose.
- Origin options: self-signed certificates, Host header, HTTP/2, TCP/SSH/RDP services…
- Live `cloudflared` log, metrics and connection status.

A single Go binary with SQLite and the UI embedded; the Docker image includes `cloudflared`.

## Getting started

```sh
docker compose up -d --build
```

Open `http://SERVER-IP:8088`, create the administrator account and follow these steps:

1. **Settings → Cloudflare account**: paste an API token. To create one, open
   <https://dash.cloudflare.com/profile/api-tokens> → *Create Token* → *Create Custom Token* and add exactly:

   | First dropdown | Second | Third | What it is for |
   |---|---|---|---|
   | Account | Cloudflare Tunnel | Edit | Create tunnels and write their routes |
   | Zone | Zone | Read | See your domains |
   | Zone | DNS | Edit | Create the DNS record for each host |
   | Account | Access: Apps and Policies | Edit | Optional: protect hosts with a sign-in |

   In Cloudflare's new dashboard (Read/Edit checkboxes) the rows are "DNS", "Zone" and "Cloudflare Tunnel"; note that
   "DNS Settings" is a different permission and does not work. As resources, include your account, and make sure the
   token's zone policy includes all the zones you want to publish; do not set an expiry. The panel includes this guide
   and a "Check permissions" button that tests each area. Changes to a token can take a couple of minutes to apply.
   If DNS is still denied, also tick "Zone Settings".
   For Access, the account must have Zero Trust enabled (the free plan is enough).

2. **Tunnels → Create tunnel**, with the connector "on this server".
3. **Hosts → Add host** for each service.

The data (database and encryption key) lives in `./data`. Back up that folder.

### Destinations and Docker networking

The destination address is resolved from wherever the connector runs. Inside the container, `localhost` is the
container itself: use the machine's IP on your network (`192.168.1.10`), `host.docker.internal` for services on the
same machine, or the container name if you share a Docker network with it.

## Migrating from Nginx Proxy Manager

1. Create each host pointing directly at the service (or at your NPM, `http://IP:80`, if you want to keep its rules).
2. The panel will detect that the domain already has an `A` record pointing to your IP and will ask you to confirm
   replacing it with the tunnel's record.
3. Once all domains respond through the tunnel, remove the NAT rule for ports 80/443 from your router.

The admin panel (port 8088) is meant for your local network. If you publish it through the tunnel, protect it with Access.

## Configuration

| Variable | Default | |
|---|---|---|
| `CTM_LISTEN` | `:8088` | Listen address |
| `CTM_DATA_DIR` | `./data` (`/data` in Docker) | Database and key |
| `CTM_SECRET_KEY` | — | Passphrase used to encrypt the tokens; if unset, `data/secret.key` is generated |
| `CTM_CLOUDFLARED_BIN` | `cloudflared` | Path to the connector binary |

## How secrets are stored

- The API token and the tunnel tokens are encrypted with AES-256-GCM before being stored in `data/ctm.db`.
- The key is in `data/secret.key` or, if you set `CTM_SECRET_KEY`, it is derived from that passphrase (so a copy of `data`
  alone is not enough to decrypt them).
- The API token is never sent back to the browser and never appears in the logs. The panel password is stored with bcrypt.

## How it works

The local database is the source of truth. On every change the panel rewrites the tunnel's remote configuration
(`ingress` rules), ensures the `CNAME` to `<id>.cfargotunnel.com` and, where applicable, the Access application (which is
created before the route is published, so a protected host is never left open). It only touches the tunnels it has
created and the DNS records of its hosts. "Sync" re-applies everything if something was changed by hand in Cloudflare.

## Development

```sh
make test       # tests against a simulated Cloudflare API
make build      # builds the UI and the ./ctm binary
make dev-api    # API on :8088
make dev-web    # Vite with hot reload on :5173
```

To work on the UI without a real account: `go run ./cmd/fakecf` and start the server with
`CTM_CF_API_BASE=http://127.0.0.1:8099`.
