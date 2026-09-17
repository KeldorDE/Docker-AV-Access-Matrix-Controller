# AV Access Matrix Controller

A small Go service that controls an AV Access HDMI matrix over Telnet and exposes it via a simple HTTP API.

## Features

- Connect to a matrix host over TCP
- Send control commands with automatic reconnect handling
- Switch HDMI input to HDMI output
- Read current output status
- Read EDID information for a given input
- Read and toggle the HDCP state of each input
- Identify the device (model, firmware, network settings) via `/device-info`
- Derive the port layout from the model name instead of assuming 4×4
- Push status changes to clients in real time via Server-Sent Events (`/events`)

## Docker

Build the image:

```bash
docker build -t av-access-matrix-controller .
```

Run it:

```bash
docker run -d --name av-access-matrix-controller \
  -p 62225:62225 \
  -e HDMI_MATRIX_IP=192.168.178.10 \
  av-access-matrix-controller
```

The container exposes port `62225`.

## Project structure

- `src` - matrix controller logic
- `src` - SSE broker, state notifier and `/events` handler
- `src` - tests (run without a real matrix)
- `go.mod` - Go module definition
- `Dockerfile` - container image definition
- `.github/workflows/` - CI/CD workflows
- `API-Command-Set_4KMX44-H2-V1.0.0-2.pdf` - Telnet command reference of the matrix

## Tests

```bash
go test ./...
go test -race ./...
```

The tests talk to a fake matrix over a local TCP socket and therefore don't
require any real AV Access hardware.

## Notes

This repository is intentionally lightweight and focuses on direct communication with the AV matrix device over a simple serial-like command protocol.


## API Endpoints

| Method | Endpoint       | Parameter                   | Description                            |
| ------ | -------------- | --------------------------- | --------------------------------------- |
| `GET`  | `/health`      | –                           | Checks the connection to the matrix     |
| `GET`  | `/device-info` | `refresh=true` (optional)   | Model, firmware, network, port count    |
| `GET`  | `/status`      | –                           | Returns routing, EDID and HDCP status   |
| `GET`  | `/status/hdcp` | –                           | Returns only the HDCP status per input  |
| `GET`  | `/events`      | –                           | Server-Sent Events stream with the full state |
| `POST` | `/switch`      | `input`, `output`           | Switches an input to an output          |
| `POST` | `/switch/hdcp` | `input`, `hdcp`             | Turns HDCP for an input on/off          |
| `POST` | `/edid`        | `input`, `edid: 1–15`       | Sets the EDID profile of an input       |

The valid ranges for `input`/`output` are derived from the detected port count
of the matrix (see `/device-info`); for the 4KMX44-H2 this is `1–4`.

The API listens on port `62225` by default.

### `/status` and `/status/hdcp`

`/status` returns the complete cache:

```json
{
  "out1_in": 1,
  "out2_in": 2,
  "out3_in": 3,
  "out4_in": 4,
  "edid_in1": 5,
  "edid_in2": 5,
  "edid_in3": 5,
  "edid_in4": 5,
  "hdcp_in1": true,
  "hdcp_in2": false,
  "hdcp_in3": true,
  "hdcp_in4": false
}
```

`/status/hdcp` returns only the `hdcp_inX` values, making it directly usable
as a `state_resource` for Home Assistant `switch` entities.

The HDCP values are booleans (`true` = HDCP active). They come from
`GET HDCP_S hdmiinX` and are updated in the same poll interval as routing and
EDID. According to the command set, HDCP support is `on` by default.

### `/events` (Server-Sent Events)

`GET /events` returns a persistent `text/event-stream`. Each `state` event
contains the **complete current controller state** – i.e. exactly the same
fields that `/status` returns. There is no separate status model for SSE and
no partial updates of individual fields.

```bash
curl -N http://127.0.0.1:62225/events
```

```
retry: 5000

event: state
data: {"out1_in":1,"out2_in":2,"out3_in":1,"out4_in":4,"edid_in1":5,"edid_in2":5,"edid_in3":5,"edid_in4":5,"hdcp_in1":true,"hdcp_in2":true,"hdcp_in3":true,"hdcp_in4":true}

: keepalive

event: state
data: {"out1_in":1,"out2_in":3,"out3_in":1,"out4_in":4,"edid_in1":5,"edid_in2":5,"edid_in3":5,"edid_in4":5,"hdcp_in1":true,"hdcp_in2":true,"hdcp_in3":true,"hdcp_in4":true}
```

Behavior in detail:

- Right after connecting, the controller sends the currently cached state.
  If no valid cache exists yet, the client waits for the next regular status
  update – a matrix query is **not** triggered for this purpose.
- A `state` event is only created if the cache actually changed. A poll with
  an unchanged result does not generate an event. If several fields change at
  the same time, there is still only one event.
- Changes made via `/switch`, `/switch/hdcp` and `/edid` are published
  immediately after the successful matrix command; there's no need to wait
  for the next poll.
- Physical switching on the front panel is detected by the fast poll within
  approximately `STATUS_POLL_INTERVAL`.
- Every `SSE_KEEPALIVE_INTERVAL` seconds, the SSE comment `: keepalive` is
  sent so that reverse proxies don't close the connection due to inactivity.
- `retry: 5000` controls the automatic reconnect of common SSE clients. Since
  every event contains the full state, there is no event history and
  `Last-Event-ID` is not needed.
- The number of connected clients has **no** effect on the number of matrix
  queries: all clients are served from the same cache.

`/status` remains unchanged and is still the right choice for
initialization, diagnostics, fallback, and clients without SSE support.

#### Home Assistant

```yaml
sensor:
  - platform: rest
    name: Matrix State
    resource: http://127.0.0.1:62225/status
    json_attributes:
      - out1_in
      - out2_in
      - out3_in
      - out4_in
    value_template: "{{ value_json.out1_in }}"
```

The REST sensor example above still polls `/status`. For real-time updates,
an integration instead subscribes to `http://127.0.0.1:62225/events` and
evaluates the `state` events – without generating any additional matrix
queries.

### Polling levels

| Level           | Variable               | Default | Commands                                    |
| --------------- | ---------------------- | ------- | -------------------------------------------- |
| Fast Status Poll| `STATUS_POLL_INTERVAL` | `3s`    | `GET MP all` (one command for all outputs)   |
| Full Sync       | `FULL_SYNC_INTERVAL`   | `60s`   | Routing, EDID, HDCP and, if needed, device info |

The fast poll only queries the input/output routing – exactly the value that
changes on physical switching. In the normal case it therefore costs a single
Telnet command per cycle, regardless of the number of outputs and connected
SSE clients.

If the matrix responds to `GET MP all` with its welcome line, it doesn't know
the batch command; the controller then permanently switches to
`GET MP hdmioutX` per output. A single timeout, however, is treated as a
transport problem: the poll only falls back to individual queries for that
cycle and then tries the batch command again afterwards. Only after several
consecutive timeouts is the batch command permanently disabled.

Fast poll, full sync and REST commands share the same Telnet connection and
the same command serialization, including `HDMI_MATRIX_COMMAND_DELAY`. No
additional connection is opened for SSE or polling.

Both poll intervals are pauses **between** two cycles, not between their
start times. If a cycle takes longer than its interval, the polls therefore
don't run into each other and don't block the connection permanently. Still,
anyone who significantly reduces `STATUS_POLL_INTERVAL` should also keep an
eye on `HDMI_MATRIX_COMMAND_DELAY`.

### `/switch/hdcp`

```bash
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"input":2,"hdcp":true}' \
  http://127.0.0.1:62225/switch/hdcp | jq
```

```json
{
  "success": true,
  "input": 2,
  "hdcp": true,
  "response": "HDCP_S hdmiin2 on"
}
```

For `hdcp`, in addition to `true`/`false`, `1`/`0` as well as the strings
`"on"`, `"off"`, `"true"`, `"false"`, `"enable"`, `"disable"`, `"yes"` and
`"no"` are also accepted so that Home Assistant templates work without extra
conversions.

If the matrix doesn't know the HDCP commands, `/status/hdcp` and
`/switch/hdcp` respond with `501` and `"error": "hdcp_unsupported"`; the
`hdcp_inX` fields are then also missing from `/status`.

#### Home Assistant example

```yaml
switch:
  - platform: rest
    name: Matrix HDCP Input 1
    resource: http://127.0.0.1:62225/switch/hdcp
    state_resource: http://127.0.0.1:62225/status/hdcp
    is_on_template: "{{ value_json.hdcp_in1 }}"
    body_on: '{"input": 1, "hdcp": true}'
    body_off: '{"input": 1, "hdcp": false}'
    headers:
      Content-Type: application/json
```

### `/edid`

Valid values (`SET EDID hdmiinX prm`, `prm = 1–15`):

| Value | Meaning                                    |
| ----- | ------------------------------------------- |
| 1–4   | Copies the EDID from output 1–4              |
| 5     | 4K@60Hz, 5.1ch audio, with HDR               |
| 6     | 4K@60Hz, 2.0ch audio, with HDR               |
| 7     | 4K@30Hz, 7.1ch audio, with HDR               |
| 8     | 4K@30Hz, 5.1ch audio, with HDR               |
| 9     | 4K@30Hz, 2.0ch audio, with HDR               |
| 10    | 4K@30Hz/8bit, 2.0ch audio, without HDR       |
| 11    | 1080p@60Hz, 2.0ch audio                      |
| 12    | 5K Ultra Wide, 2ch audio                     |
| 13    | 5K Super Wide, 2ch audio                     |
| 14    | Smart EDID                                   |
| 15    | EDID Write                                   |

The included Command Set V1.0.0 lists only `1–12` here, which is outdated;
the selection in the matrix's web interface is authoritative.

### `/device-info`

```bash
curl -s http://127.0.0.1:62225/device-info | jq
```

```json
{
  "model": "4KMX44-H2",
  "manufacturer": "AV Access",
  "unique_id": "4kmx44-h2-192-168-178-10",
  "sw_version": "1.0",
  "hw_version": null,
  "arm_version": "1.0",
  "configuration_url": "http://192.168.1.4",
  "ip_address": "192.168.1.4",
  "netmask": "255.255.255.0",
  "gateway": "192.168.1.1",
  "ip_mode": "dhcp",
  "input_count": 4,
  "output_count": 4,
  "host": "192.168.178.10",
  "port": 23,
  "updated_at": "2026-09-16T13:37:00Z"
}
```

The data is read once from the matrix at startup and cached. `?refresh=true`
forces a fresh query. As long as no connection has been established yet, the
endpoint responds with `503`.

#### Field origins

| Field                              | Source                                                       |
| ---------------------------------- | ------------------------------------------------------------- |
| `model`                            | Welcome line on connect, confirmed by `GET VER`                |
| `sw_version`, `arm_version`        | `GET VER` (`4KMX44-H2 VER 1.0, ARM VER 1.0`)                    |
| `ip_address`, `netmask`, `gateway` | `GET IPADDR`                                                    |
| `ip_mode`                          | `GET IP MODE`                                                   |
| `input_count`, `output_count`      | derived from the model name (`4KMX44` → 4×4)                    |
| `configuration_url`                | built from the reported matrix IP                               |
| `hw_version`                       | not provided for in the command set → always `null`             |

#### Note on `unique_id`

The command set of the 4KMX44-H2 does **not** have a command for serial
number or MAC address (only `RESET`, `REBOOT`, `help`, `SET/GET IP MODE`,
`SET/GET IPADDR`, `GET VER` and `UPG` are documented). Therefore there is no
`serial` field.

`unique_id` is built from `<model>-<configured host>`, e.g.
`4kmx44-h2-192-168-178-10`. It is thus stable as long as the matrix is
reachable under the same address.

## Configuration

| Variable                        | Default         | Description                                             |
| -------------------------------- | --------------- | -------------------------------------------------------- |
| `HDMI_MATRIX_IP`                | `192.168.178.10`| Address of the matrix                                     |
| `HDMI_MATRIX_PORT`              | `23`            | Telnet port                                                |
| `HDMI_MATRIX_TIMEOUT`           | `2`             | Read/write timeout in seconds                              |
| `HDMI_MATRIX_COMMAND_DELAY`     | `1`             | Minimum spacing between commands in seconds                |
| `STATUS_POLL_INTERVAL`          | `3s`            | Fast poll of routing (detects front panel switching)       |
| `FULL_SYNC_INTERVAL`            | `60s`           | Full sync including EDID and HDCP                          |
| `SSE_KEEPALIVE_INTERVAL`        | `30s`           | Spacing of `: keepalive` comments in the SSE stream         |
| `HDMI_MATRIX_INPUTS`            | auto            | Overrides the detected number of inputs                    |
| `HDMI_MATRIX_OUTPUTS`           | auto            | Overrides the detected number of outputs                   |
| `HDMI_MATRIX_CONFIGURATION_URL` | –               | Overrides the automatic `configuration_url`                |
| `HTTP_HOST` / `HTTP_PORT`       | `0.0.0.0`/`62225` | Address of the HTTP server                                |
| `LOG_LEVEL`                     | `INFO`          | `DEBUG` logs the Telnet traffic                             |

All time values accept plain seconds (`60`, `0.5`) as well as Go duration
strings (`3s`, `1m30s`).

`HDMI_MATRIX_STATUS_POLL_INTERVAL` remains valid as an alias for
`FULL_SYNC_INTERVAL` so that existing deployments keep working unchanged. If
both are set, `FULL_SYNC_INTERVAL` wins.

#### Examples

```bash
# Device information
curl -s http://127.0.0.1:62225/device-info | jq

# Status
curl -s http://127.0.0.1:62225/status | jq

# Input 2 to output 3
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"input":2,"output":3}' \
  http://127.0.0.1:62225/switch | jq

# EDID 10 for input 4
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"input":4,"edid":10}' \
  http://127.0.0.1:62225/edid | jq

# HDCP status of all inputs
curl -s http://127.0.0.1:62225/status/hdcp | jq

# Disable HDCP for input 1
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"input":1,"hdcp":false}' \
  http://127.0.0.1:62225/switch/hdcp | jq

# Follow status changes live
curl -N http://127.0.0.1:62225/events
```
