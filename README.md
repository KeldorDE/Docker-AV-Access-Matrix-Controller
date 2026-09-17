# AV Access Matrix Controller

A small Python helper for controlling an AV matrix switch over a TCP connection.

This project provides a simple `MatrixConnection` class that can connect to a matrix device, switch inputs/outputs, and read EDID or output status values.

## Features

- Connect to a matrix host over TCP
- Send control commands with automatic reconnect handling
- Switch HDMI input to HDMI output
- Read current output status
- Read EDID information for a given input
- Validate allowed input/output ranges

## Example

```python
from scripts.app import MatrixConnection

matrix = MatrixConnection("192.168.1.10", 23, timeout=5.0)

matrix.switch(1, 2)
print(matrix.get_output(2))
print(matrix.get_edid(1))
```

## Docker

Build the image:

```bash
docker build -t av-access-matrix-controller .
```

The container is configured to run the Python application in `/app` and exposes port `62225`.

## Project structure

- `scripts/app.py` - matrix controller logic
- `Dockerfile` - container image definition
- `.github/workflows/` - CI/CD workflows

## Notes

This repository is intentionally lightweight and focuses on direct communication with the AV matrix device over a simple serial-like command protocol.


## API Endpoints

| Method | Endpoint  | Parameter                   | Beschreibung                          |
| ------ | --------- | --------------------------- | ------------------------------------- |
| `GET`  | `/health` | –                           | Prüft die Verbindung zur Matrix       |
| `GET`  | `/status` | –                           | Liefert Routing- und EDID-Status      |
| `POST` | `/switch` | `input: 1–4`, `output: 1–4` | Schaltet einen Input auf einen Output |
| `POST` | `/edid`   | `input: 1–4`, `edid: 1–15`  | Setzt das EDID-Profil eines Inputs    |

Die API lauscht standardmäßig auf Port `62225`.

#### Beispiele

```bash
# Status
curl -s http://127.0.0.1:62225/status | jq

# Input 2 auf Output 3
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"input":2,"output":3}' \
  http://127.0.0.1:62225/switch | jq

# EDID 4 für Input 4
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"input":4,"edid":4}' \
  http://127.0.0.1:62225/edid | jq
```
