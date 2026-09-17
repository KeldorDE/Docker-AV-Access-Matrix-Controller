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

- `scripts/app.go` - matrix controller logic
- `Dockerfile` - container image definition
- `.github/workflows/` - CI/CD workflows
- `API-Command-Set_4KMX44-H2-V1.0.0-2.pdf` - Telnet command reference of the matrix

## Notes

This repository is intentionally lightweight and focuses on direct communication with the AV matrix device over a simple serial-like command protocol.


## API Endpoints

| Method | Endpoint       | Parameter                   | Beschreibung                          |
| ------ | -------------- | --------------------------- | ------------------------------------- |
| `GET`  | `/health`      | –                           | Prüft die Verbindung zur Matrix       |
| `GET`  | `/device-info` | `refresh=true` (optional)   | Modell, Firmware, Netzwerk, Portanzahl |
| `GET`  | `/status`      | –                           | Liefert Routing-, EDID- und HDCP-Status |
| `GET`  | `/status/hdcp` | –                           | Liefert nur den HDCP-Status je Input  |
| `POST` | `/switch`      | `input`, `output`           | Schaltet einen Input auf einen Output |
| `POST` | `/switch/hdcp` | `input`, `hdcp`             | Schaltet HDCP eines Inputs an/aus     |
| `POST` | `/edid`        | `input`, `edid: 1–15`       | Setzt das EDID-Profil eines Inputs    |

Die gültigen Bereiche für `input`/`output` ergeben sich aus der erkannten
Portanzahl der Matrix (siehe `/device-info`), beim 4KMX44-H2 also `1–4`.

Die API lauscht standardmäßig auf Port `62225`.

### `/status` und `/status/hdcp`

`/status` liefert den kompletten Cache:

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

`/status/hdcp` liefert nur die `hdcp_inX`-Werte und eignet sich damit direkt als
`state_resource` für Home-Assistant-`switch`-Entitäten.

Die HDCP-Werte sind boolesch (`true` = HDCP aktiv). Sie stammen aus
`GET HDCP_S hdmiinX` und werden im selben Poll-Intervall wie Routing und EDID
aktualisiert. Laut Command Set ist HDCP-Support ab Werk `on`.

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

Für `hdcp` werden neben `true`/`false` auch `1`/`0` sowie die Strings `"on"`,
`"off"`, `"true"`, `"false"`, `"enable"`, `"disable"`, `"yes"` und `"no"`
akzeptiert, damit Home-Assistant-Templates ohne Umwege funktionieren.

Kennt die Matrix die HDCP-Kommandos nicht, antworten `/status/hdcp` und
`/switch/hdcp` mit `501` und `"error": "hdcp_unsupported"`; die `hdcp_inX`-Felder
fehlen dann auch in `/status`.

#### Home Assistant Beispiel

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

Gültige Werte (`SET EDID hdmiinX prm`, `prm = 1–15`):

| Wert | Bedeutung                                |
| ---- | ---------------------------------------- |
| 1–4  | Kopiert das EDID von Output 1–4          |
| 5    | 4K@60Hz, 5.1ch Audio, mit HDR            |
| 6    | 4K@60Hz, 2.0ch Audio, mit HDR            |
| 7    | 4K@30Hz, 7.1ch Audio, mit HDR            |
| 8    | 4K@30Hz, 5.1ch Audio, mit HDR            |
| 9    | 4K@30Hz, 2.0ch Audio, mit HDR            |
| 10   | 4K@30Hz/8bit, 2.0ch Audio, ohne HDR      |
| 11   | 1080p@60Hz, 2.0ch Audio                  |
| 12   | 5K Ultra Wide, 2ch Audio                 |
| 13   | 5K Super Wide, 2ch Audio                 |
| 14   | Smart EDID                               |
| 15   | EDID Write                               |

Das mitgelieferte Command Set V1.0.0 listet an dieser Stelle veraltet nur
`1–12`; maßgeblich ist die Auswahl im Web-Interface der Matrix.

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

Die Daten werden beim Start einmalig von der Matrix gelesen und zwischengespeichert.
`?refresh=true` erzwingt eine erneute Abfrage. Solange noch keine Verbindung
zustande kam, antwortet der Endpoint mit `503`.

#### Herkunft der Felder

| Feld                              | Quelle                                                      |
| --------------------------------- | ----------------------------------------------------------- |
| `model`                           | Welcome-Zeile beim Connect, bestätigt durch `GET VER`        |
| `sw_version`, `arm_version`       | `GET VER` (`4KMX44-H2 VER 1.0, ARM VER 1.0`)                 |
| `ip_address`, `netmask`, `gateway`| `GET IPADDR`                                                 |
| `ip_mode`                         | `GET IP MODE`                                                |
| `input_count`, `output_count`     | aus dem Modellnamen abgeleitet (`4KMX44` → 4×4)              |
| `configuration_url`               | aus der gemeldeten Matrix-IP gebildet                        |
| `hw_version`                      | im Command Set nicht vorgesehen → immer `null`               |

#### Hinweis zu `unique_id`

Das Command Set des 4KMX44-H2 kennt **kein** Kommando für Seriennummer oder
MAC-Adresse (dokumentiert sind nur `RESET`, `REBOOT`, `help`, `SET/GET IP MODE`,
`SET/GET IPADDR`, `GET VER` und `UPG`). Deshalb gibt es kein `serial`-Feld.

`unique_id` wird aus `<model>-<konfigurierter host>` gebildet, z. B.
`4kmx44-h2-192-168-178-10`. Sie ist damit stabil, solange die Matrix unter
derselben Adresse erreichbar ist.

## Konfiguration

| Variable                        | Default         | Beschreibung                                         |
| ------------------------------- | --------------- | ---------------------------------------------------- |
| `HDMI_MATRIX_IP`                | `192.168.178.10`| Adresse der Matrix                                   |
| `HDMI_MATRIX_PORT`              | `23`            | Telnet-Port                                          |
| `HDMI_MATRIX_TIMEOUT`           | `2`             | Lese-/Schreib-Timeout in Sekunden                    |
| `HDMI_MATRIX_COMMAND_DELAY`     | `1`             | Mindestabstand zwischen Kommandos in Sekunden        |
| `HDMI_MATRIX_STATUS_POLL_INTERVAL` | `60`         | Poll-Intervall des Status-Caches in Sekunden         |
| `HDMI_MATRIX_INPUTS`            | auto            | Überschreibt die erkannte Anzahl Eingänge            |
| `HDMI_MATRIX_OUTPUTS`           | auto            | Überschreibt die erkannte Anzahl Ausgänge            |
| `HDMI_MATRIX_CONFIGURATION_URL` | –               | Überschreibt die automatische `configuration_url`    |
| `HTTP_HOST` / `HTTP_PORT`       | `0.0.0.0`/`62225` | Adresse des HTTP-Servers                           |
| `LOG_LEVEL`                     | `INFO`          | `DEBUG` protokolliert den Telnet-Verkehr             |

#### Beispiele

```bash
# Geräteinformationen
curl -s http://127.0.0.1:62225/device-info | jq

# Status
curl -s http://127.0.0.1:62225/status | jq

# Input 2 auf Output 3
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"input":2,"output":3}' \
  http://127.0.0.1:62225/switch | jq

# EDID 10 für Input 4
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"input":4,"edid":10}' \
  http://127.0.0.1:62225/edid | jq

# HDCP-Status aller Inputs
curl -s http://127.0.0.1:62225/status/hdcp | jq

# HDCP für Input 1 deaktivieren
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"input":1,"hdcp":false}' \
  http://127.0.0.1:62225/switch/hdcp | jq
```
