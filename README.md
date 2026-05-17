# glsms

A small Go library, REST server, and CLI for **reading and sending SMS**
through a GL.iNet cellular router's JSON-RPC interface (firmware 4.x — tested
on a **GL-X3000 / Spitz AX** at `192.168.8.1`).

The router's web UI does not document this; the SMS RPC protocol below was
discovered by probing a live X3000 and is what this tool implements.

## Layout

| Path         | What it is                                           |
| ------------ | ---------------------------------------------------- |
| `glsms/`     | Reusable library (`Client`, `SMS`, REST `Handler`)   |
| `tui/`       | Interactive terminal UI (Bubble Tea), reuses `glsms` |
| `cmd/glsms/` | CLI binary — also `serve` (REST) and `tui`           |

## Build

```powershell
go build -o bin/glsms.exe ./cmd/glsms
```

## Configuration

All entry points read connection settings from flags or environment:

| Env           | Flag     | Default       | Notes                                 |
| ------------- | -------- | ------------- | ------------------------------------- |
| `GL_HOST`     | `-host`  | `192.168.8.1` | Router host or full URL               |
| `GL_USER`     | `-user`  | `root`        | Router admin username                 |
| `GL_PASS`     | `-pass`  | _(required)_  | Router admin password                 |
| `GLSMS_ADDR`  | `-addr`  | `:8080`       | `serve` listen address                |
| `GLSMS_TOKEN` | `-token` | _(none)_      | If set, required as Bearer on `/api/` |

## CLI

```powershell
$env:GL_PASS = "yourpassword"

glsms status                              # SIM / signal / new-message count
glsms list                                # table of stored messages (with direction)
glsms list -dir received                  # only received messages
glsms list -dir sent                      # only sent messages
glsms -json list                          # same, as JSON
glsms send   -to +15551234567 -body "hi"  # send an SMS
glsms send   -to +1555… -body "hi" -timeout 120  # wait up to 120s for confirm
glsms send   -to +1555… -body "hi" -timeout 0    # fire-and-forget, don't wait
glsms read   -name GMS_xxxxxx             # mark a message read
glsms unread -name GMS1.xxxxxx            # mark a received message unread
glsms delete -name GMS_xxxxxx             # delete a message
glsms tui                                 # interactive terminal UI
glsms serve  -addr :8080                  # run the REST API
```

`-name` values come from the `name` column of `glsms list` — that is the
router's opaque per-message storage key.

`send -timeout` is how many seconds the **router** waits for the modem to
confirm delivery before returning an error (default **60**; `0` returns
immediately without waiting). On weak signal or a slow carrier, raise it. See
[Timeouts](#timeouts).

## TUI

`glsms tui` opens an interactive full-screen terminal UI (Bubble Tea) over the
same library. It must be run in a real terminal — when stdin/stdout is not a
TTY it exits immediately with a message instead of hanging.

Header shows SIM number, carrier, network, signal bars, new-message count, and
the auto-refresh state. **Auto-refresh is on by default**: every 2 seconds the
message list/status reload silently (no spinner, skipped while busy or while a
slower reload is still running). Toggle it with `a`.

The list screen is split into two side-by-side panes — **Inbox** (received) on
the left and **Outbox** (sent) on the right — each with its own cursor; the
active pane has a highlighted border. Unread (new) received messages stand out:
a bright-yellow `N new` badge in the Inbox header, plus each unread row shown in
bold bright yellow with a leading `●` (read rows are dimmed). **Opening a
message (`enter`) marks it read**; press `u` (in the list or detail view) to
mark a received message unread again. Read/unread changes apply optimistically
and keep you on the current screen — the detail view shows the live state
(`received · UNREAD` / `received · read` / `sent`).

Keys:

| Screen         | Keys                                                                                                                                                                                                                                      |
| -------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| List           | `←/→`(`h/l`,`tab`) switch pane · `↑/↓`(`j/k`) move · `enter` view (**marks read**) · `r` reply (Inbox only) · `c` compose · `m` mark read · `u` mark unread · `d` delete · `a` toggle auto-refresh · `ctrl+r`/`F5` refresh now · `q` quit |
| Detail         | `↑/↓` scroll · `m` mark read · `u` mark unread · `r` reply (received only) · `d` delete · `a` toggle auto-refresh · `esc` back                                                                                                            |
| Compose        | `tab` switch To/Message · `ctrl+s` send · `esc` cancel                                                                                                                                                                                    |
| Confirm delete | `y` confirm · `n` cancel                                                                                                                                                                                                                  |

**Reply** (`r` on an Inbox message, or in its detail view) opens Compose with
the recipient pre-filled to that sender's number and the cursor in the message
body; the screen shows a "Reply to …" heading.

All actions go through the same `glsms.SMS` methods as the CLI/REST (same auth,
two-step delete, direction detection); the list auto-refreshes after a send,
delete, or mark-read.

## REST API

Start it: `glsms serve -addr :8080` (optionally `-token <secret>`).

| Method & path           | Body / query                           | Description                                         |
| ----------------------- | -------------------------------------- | --------------------------------------------------- |
| `GET /healthz`          | —                                      | Liveness (no auth, no RPC)                          |
| `GET /api/status`       | —                                      | Modem/SIM status + new count                        |
| `GET /api/sms`          | `?direction=sent\|received` (optional) | `{ "messages": [ ... ] }`                           |
| `POST /api/sms`         | `{"to","body","timeout?"}`             | Send an SMS (`timeout` default 60s, `0`=don't wait) |
| `POST /api/sms/read`    | `{"name"}`                             | Mark message read                                   |
| `POST /api/sms/unread`  | `{"name"}`                             | Mark received message unread                        |
| `DELETE /api/sms?name=` | `?name=GMS_xxxx`                       | Delete message                                      |

If `-token` / `GLSMS_TOKEN` is set, send `Authorization: Bearer <token>` on
every `/api/` request.

Example:

```bash
curl -s localhost:8080/api/sms | jq
curl -s 'localhost:8080/api/sms?direction=received' | jq
curl -s -X POST localhost:8080/api/sms \
  -H 'content-type: application/json' \
  -d '{"to":"+15551234567","body":"hello","timeout":60}'
curl -s -X DELETE 'localhost:8080/api/sms?name=GMS_xxxxxx'
```

## Library

```go
c   := glsms.New("192.168.8.1", "root", os.Getenv("GL_PASS"))
sms := glsms.NewSMS(c)

st, _   := sms.Status(ctx)          // ModemStatus (SIM, signal, NewSMSCount)
msgs, _ := sms.List(ctx)            // []Message
_        = sms.Send(ctx, "+1555…", "hi", 60) // last arg: router wait seconds
_        = sms.MarkRead(ctx, name)
_        = sms.MarkUnread(ctx, name)
_, _     = sms.Delete(ctx, name)    // returns remaining messages
```

`glsms.Handler(sms, glsms.ServerConfig{AuthToken: "..."})` returns the
`http.Handler` used by `serve`, so the REST API can be embedded in another
program.

## Timeouts

A real `send_sms` blocks on the router until the modem confirms delivery (or
the carrier gives up). If that wait is shorter than the modem needs, the router
returns `Send sms timeout` (err 20002039). The defaults are deliberately
generous and layered so the _innermost_ (router-side) wait is the one that
actually governs:

| Layer                                            | Default                         | Configure                                                                   |
| ------------------------------------------------ | ------------------------------- | --------------------------------------------------------------------------- |
| Router modem-confirm wait (`send_sms` `timeout`) | **60s**                         | CLI `-timeout`, REST `{"timeout":N}`, `SMS.Send` last arg; `0` = don't wait |
| RPC HTTP client timeout                          | 180s                            | `glsms.WithHTTPClient(...)`                                                 |
| CLI `send` context                               | `timeout + 120s`                | — (derived)                                                                 |
| CLI other commands context                       | 120s                            | —                                                                           |
| REST `ServerConfig.CallTimeout`                  | 120s                            | `ServerConfig{CallTimeout: …}`                                              |
| REST `POST /api/sms` context                     | `max(CallTimeout, timeout+60s)` | — (derived)                                                                 |
| REST server `WriteTimeout`                       | 300s                            | `cmd/glsms` `newServer`                                                     |

So to allow a longer send, raise just the innermost value
(`-timeout` / `{"timeout":N}`); the outer layers already accommodate it. If you
embed the library with a very large send timeout, also pass a matching
`WithHTTPClient` so the 180s HTTP cap doesn't cut it short.

## Discovered GL.iNet 4.x SMS RPC protocol

Endpoint: `POST http://<router>/rpc`, JSON-RPC 2.0.

### Authentication (challenge / response)

1. `challenge` with `{"username":"root"}` →
   `{"alg":1,"salt":"…","nonce":"…","hash-method":"sha256"}`.
   `alg` selects the crypt scheme: `1`=MD5 (`$1$`), `5`=SHA-256 (`$5$`),
   `6`=SHA-512 (`$6$`).
2. `hashed = crypt(password, "$<alg>$<salt>")` (Unix crypt).
3. `loginHash = sha256_hex(username + ":" + hashed + ":" + nonce)`.
4. `login` with `{"username","hash":loginHash}` → `{"sid":"…"}`.
5. All further calls: method `"call"`, params `[sid, service, method, args]`.

A stale session returns RPC error `-32000`; the client re-logs in and retries
once automatically.

### SMS lives under the `modem` service

The `sms` service does **not** exist on this firmware; SMS is part of `modem`.
The active modem **bus** (e.g. `0001:01:00.0`) is required by every SMS call
and is discovered from `modem.get_status`.

| Call                 | Params                                    | Result                                                                                 |
| -------------------- | ----------------------------------------- | -------------------------------------------------------------------------------------- |
| `modem.get_status`   | `{}`                                      | `{ "modems":[{ "bus", "simcard":{…}, … }], "new_sms_count": N }`                       |
| `modem.get_sms_list` | `{"bus"}`                                 | `{ "list":[{ "name","phone_number","sender","body","date","status","type?","bus" }] }` |
| `modem.send_sms`     | `{"bus","phone_number","body","timeout"}` | `[]` on success                                                                        |
| `modem.set_sms`      | `{"bus","name","status"}`                 | `[]`; sets a message's status code                                                     |
| `modem.remove_sms`   | `{"bus","name"}`                          | `[]`; deletes a _flagged_ message                                                      |

**Quirks discovered:**

- Logical failures come back as **HTTP 200** with
  `{"err_msg":"…","err_code":N}`, not a JSON-RPC error. Success payloads are
  usually a JSON array (`[]`). The client treats only the `err_*` object as an
  error.
- `date` is `YY-MM-DD HH:MM:SS` (e.g. `26-05-14 21:44:42`), parsed as local
  time.
- Message `status` is the standard GSM `+CMGL` code: **`0` = received &
  unread**, **`1` = received & read**, **`2` = sent**. Verified against the
  X3000: the number of status-`0` messages equals `modem.get_status`'s
  `new_sms_count`. (`status 0` also doubles as the "removable" flag for
  deletion — see below.)
- **There is no explicit sent/received flag.** The firmware instead reveals it
  two ways, which agree: received messages are stored on the SIM with a name
  like `GMS1.xxxx` (`GMS` + digits + `.`) and include a `type` field (value
  `0`); messages this device sent are stored in modem memory with a name like
  `GMS_xxxx` and **omit** `type`. `glsms` derives `Message.Direction`
  (`received`/`sent`/`unknown`) from these: `type` present ⇒ received; else
  `GMS_` prefix ⇒ sent; else SIM-style name ⇒ received. For a sent message
  `phone_number` is the recipient; for a received one it's the sender. The
  `-dir` / `?direction=` filters operate on this derived value. (Heuristic,
  not documented by GL.iNet — verified on an X3000 but adjust `DirectionOf`
  if a future firmware differs.)
- **Deleting requires two steps:** `set_sms(status=0)` _then_ `remove_sms`.
  Calling `remove_sms` alone never deletes. Removal also is not instantaneous,
  so `SMS.Delete` issues both calls and then polls `get_sms_list` until the
  message is gone (giving up after ~36s).
- There is **no mark-read RPC**; "read" is just `set_sms(status=1)` (received
  read).
- `send_sms` blocks server-side for its `timeout` param and returns
  `{"err_code":20002039,"err_msg":"Send sms timeout"}` if the modem doesn't
  confirm in time — raise the send timeout (see [Timeouts](#timeouts)).
- Sending to the SIM's own number loops back as a received message (at least
  on the carrier it was tested with), which is how send was verified
  end-to-end. Carrier behaviour here may vary.

## Notes / limitations

- `SMS.Delete` confirms removal by polling; on a busy modem the confirmation
  window may need widening.
- No multi-SIM switching (firmware RPC doesn't expose it).
- Keep the router password out of source — use `GL_PASS`. `.gitignore`
  excludes `.secrets/`, `config.json`, `.env`, and build output. No
  credentials are stored in the repo.
- All phone numbers, message bodies, and storage names in this README and in
  the tests are fabricated examples (e.g. `+15551234567`). The protocol
  details were reverse-engineered from a GL-X3000 on firmware 4.x.

## License

[MIT](LICENSE).
