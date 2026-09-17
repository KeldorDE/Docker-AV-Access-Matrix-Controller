import json
import logging
import os
import re
import signal
import socket
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse


# -------------------------------------------------------------------
# Configuration
# -------------------------------------------------------------------

MATRIX_HOST = os.getenv("MATRIX_HOST", "192.168.178.10")
MATRIX_PORT = int(os.getenv("MATRIX_PORT", "23"))
MATRIX_TIMEOUT = float(os.getenv("MATRIX_TIMEOUT", "2"))

HTTP_HOST = os.getenv("HTTP_HOST", "0.0.0.0")
HTTP_PORT = int(os.getenv("HTTP_PORT", "62225"))

LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()


# -------------------------------------------------------------------
# Logging
# -------------------------------------------------------------------

logging.basicConfig(
    level=LOG_LEVEL,
    format="%(asctime)s %(levelname)s %(message)s",
)

logger = logging.getLogger("av-access-controller")


# -------------------------------------------------------------------
# Matrix connection
# -------------------------------------------------------------------

class MatrixConnection:
    def __init__(self, host: str, port: int, timeout: float):
        self.host = host
        self.port = port
        self.timeout = timeout

        self.socket: socket.socket | None = None
        self.reader = None

        # Es darf immer nur EIN Kommando gleichzeitig über die
        # persistente Matrix-Verbindung laufen.
        self.lock = threading.Lock()

    def connect(self):
        self.close()

        logger.info(
            "Connecting to matrix %s:%s",
            self.host,
            self.port,
        )

        sock = socket.create_connection(
            (self.host, self.port),
            timeout=self.timeout,
        )

        sock.settimeout(self.timeout)

        self.socket = sock
        self.reader = sock.makefile("rb")

        logger.info("Connected to matrix")

    def close(self):
        if self.reader is not None:
            try:
                self.reader.close()
            except Exception:
                pass

        if self.socket is not None:
            try:
                self.socket.shutdown(socket.SHUT_RDWR)
            except Exception:
                pass

            try:
                self.socket.close()
            except Exception:
                pass

        self.reader = None
        self.socket = None

    def ensure_connected(self):
        if self.socket is None:
            self.connect()

    def _readline(self) -> str:
        if self.reader is None:
            raise ConnectionError("Matrix connection is not open")

        response = self.reader.readline()

        if not response:
            raise ConnectionError("Matrix closed connection")

        decoded = response.decode(
            "ascii",
            errors="replace",
        ).strip()

        logger.debug("RX: %s", decoded)

        return decoded

    def _send(
        self,
        command: str,
        expected_pattern: str,
    ) -> str:
        self.ensure_connected()

        if self.socket is None:
            raise ConnectionError("Matrix connection is not open")

        payload = f"{command}\r\n".encode("ascii")

        logger.debug("TX: %s", command)

        self.socket.sendall(payload)

        # Die Matrix kann noch eine ältere Antwort im TCP-Puffer haben.
        # Deshalb nicht einfach die erste Zeile zurückgeben, sondern
        # so lange lesen, bis die Antwort zum aktuellen Kommando passt.
        while True:
            response = self._readline()

            if re.fullmatch(
                expected_pattern,
                response,
                re.IGNORECASE,
            ):
                return response

            logger.debug(
                "Ignoring unexpected response: %r "
                "(expected: %s)",
                response,
                expected_pattern,
            )

    def command(
        self,
        command: str,
        expected_pattern: str,
    ) -> str:
        with self.lock:
            try:
                return self._send(
                    command,
                    expected_pattern,
                )

            except (
                ConnectionError,
                BrokenPipeError,
                ConnectionResetError,
                socket.timeout,
                OSError,
            ) as exc:
                logger.warning(
                    "Matrix connection failed: %s; reconnecting",
                    exc,
                )

                self.close()
                self.connect()

                return self._send(
                    command,
                    expected_pattern,
                )

    # ---------------------------------------------------------------
    # Routing
    # ---------------------------------------------------------------

    def switch(
        self,
        input_number: int,
        output_number: int,
    ) -> int:
        self._validate_input(input_number)
        self._validate_output(output_number)

        with self.lock:
            try:
                self.ensure_connected()

                if self.socket is None:
                    raise ConnectionError("Matrix connection is not open")

                # SET senden, aber keine bestimmte Antwort erwarten.
                command = (
                    f"SET SW hdmiin{input_number} "
                    f"hdmiout{output_number}"
                )

                logger.debug("TX: %s", command)

                self.socket.sendall(
                    f"{command}\r\n".encode("ascii")
                )

                # Anschließend aktuellen Zustand abfragen.
                command = f"GET MP hdmiout{output_number}"

                logger.debug("TX: %s", command)

                self.socket.sendall(
                    f"{command}\r\n".encode("ascii")
                )

                expected_pattern = (
                    rf"MP\s+in{input_number}\s+"
                    rf"hdmiout{output_number}"
                )

                while True:
                    response = self._readline()

                    if re.fullmatch(
                        expected_pattern,
                        response,
                        re.IGNORECASE,
                    ):
                        return input_number

                    logger.debug(
                        "Ignoring response while verifying switch: %r",
                        response,
                    )

            except (
                ConnectionError,
                BrokenPipeError,
                ConnectionResetError,
                socket.timeout,
                OSError,
            ):
                self.close()
                raise

    def get_output(
        self,
        output_number: int,
    ) -> int:
        self._validate_output(output_number)

        response = self.command(
            f"GET MP hdmiout{output_number}",
            rf"MP\s+in[1-4]\s+hdmiout{output_number}",
        )

        match = re.fullmatch(
            rf"MP\s+in([1-4])\s+hdmiout{output_number}",
            response,
            re.IGNORECASE,
        )

        if not match:
            raise RuntimeError(
                f"Unexpected matrix response: {response}"
            )

        return int(match.group(1))

    # ---------------------------------------------------------------
    # EDID
    # ---------------------------------------------------------------

    def set_edid(
        self,
        input_number: int,
        edid: int,
    ) -> str:
        self._validate_input(input_number)

        if not 1 <= edid <= 15:
            raise ValueError(
                "EDID must be between 1 and 15"
            )

        return self.command(
            f"SET EDID hdmiin{input_number} {edid}",
            rf"EDID\s+hdmiin{input_number}\s+{edid}",
        )

    def get_edid(
        self,
        input_number: int,
    ) -> int:
        self._validate_input(input_number)

        response = self.command(
            f"GET EDID hdmiin{input_number}",
            rf"EDID\s+hdmiin{input_number}\s+\d+",
        )

        match = re.fullmatch(
            rf"EDID\s+hdmiin{input_number}\s+(\d+)",
            response,
            re.IGNORECASE,
        )

        if not match:
            raise RuntimeError(
                f"Unexpected matrix response: {response}"
            )

        return int(match.group(1))

    # ---------------------------------------------------------------
    # Validation
    # ---------------------------------------------------------------

    @staticmethod
    def _validate_input(number: int):
        if not 1 <= number <= 4:
            raise ValueError(
                "Input must be between 1 and 4"
            )

    @staticmethod
    def _validate_output(number: int):
        if not 1 <= number <= 4:
            raise ValueError(
                "Output must be between 1 and 4"
            )


matrix = MatrixConnection(
    MATRIX_HOST,
    MATRIX_PORT,
    MATRIX_TIMEOUT,
)


# -------------------------------------------------------------------
# HTTP API
# -------------------------------------------------------------------

class RequestHandler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        logger.debug(
            "%s - %s",
            self.address_string(),
            format % args,
        )

    def send_json(
        self,
        status: int,
        data: dict,
    ):
        body = json.dumps(
            data,
            separators=(",", ":"),
        ).encode("utf-8")

        self.send_response(status)

        self.send_header(
            "Content-Type",
            "application/json",
        )

        self.send_header(
            "Content-Length",
            str(len(body)),
        )

        self.end_headers()

        self.wfile.write(body)

    def read_json(self) -> dict:
        length = int(
            self.headers.get(
                "Content-Length",
                "0",
            )
        )

        if length == 0:
            return {}

        raw = self.rfile.read(length)

        return json.loads(
            raw.decode("utf-8")
        )

    # ---------------------------------------------------------------
    # GET
    # ---------------------------------------------------------------

    def do_GET(self):
        path = urlparse(self.path).path

        try:
            if path == "/health":
                self.handle_health()
                return

            if path == "/status":
                self.handle_status()
                return

            self.send_json(
                404,
                {
                    "error": "not_found",
                },
            )

        except Exception as exc:
            logger.exception(
                "GET %s failed",
                path,
            )

            self.send_json(
                500,
                {
                    "error": "matrix_error",
                    "message": str(exc),
                },
            )

    # ---------------------------------------------------------------
    # POST
    # ---------------------------------------------------------------

    def do_POST(self):
        path = urlparse(self.path).path

        try:
            body = self.read_json()

            if path == "/switch":
                self.handle_switch(body)
                return

            if path == "/edid":
                self.handle_edid(body)
                return

            self.send_json(
                404,
                {
                    "error": "not_found",
                },
            )

        except (
            ValueError,
            KeyError,
            json.JSONDecodeError,
        ) as exc:
            self.send_json(
                400,
                {
                    "error": "invalid_request",
                    "message": str(exc),
                },
            )

        except Exception as exc:
            logger.exception(
                "POST %s failed",
                path,
            )

            self.send_json(
                500,
                {
                    "error": "matrix_error",
                    "message": str(exc),
                },
            )

    # ---------------------------------------------------------------
    # Handlers
    # ---------------------------------------------------------------

    def handle_health(self):
        try:
            matrix.get_output(1)

            self.send_json(
                200,
                {
                    "status": "ok",
                },
            )

        except Exception as exc:
            self.send_json(
                503,
                {
                    "status": "unavailable",
                    "message": str(exc),
                },
            )

    def handle_switch(
        self,
        body: dict,
    ):
        input_number = int(
            body["input"]
        )

        output_number = int(
            body["output"]
        )

        response = matrix.switch(
            input_number,
            output_number,
        )

        self.send_json(
            200,
            {
                "success": True,
                "input": input_number,
                "output": output_number,
                "response": response,
            },
        )

    def handle_edid(
        self,
        body: dict,
    ):
        input_number = int(
            body["input"]
        )

        edid = int(
            body["edid"]
        )

        response = matrix.set_edid(
            input_number,
            edid,
        )

        self.send_json(
            200,
            {
                "success": True,
                "input": input_number,
                "edid": edid,
                "response": response,
            },
        )

    def handle_status(self):
        result = {}

        with matrix.lock:
            matrix.ensure_connected()

            commands = []

            for output_number in range(1, 5):
                commands.append(
                    f"GET MP hdmiout{output_number}"
                )

            for input_number in range(1, 5):
                commands.append(
                    f"GET EDID hdmiin{input_number}"
                )

            # Alle 8 Kommandos auf einmal senden
            payload = "".join(
                f"{command}\r\n"
                for command in commands
            ).encode("ascii")

            logger.debug("TX pipelined: %s", commands)
            matrix.socket.sendall(payload)

            # 4 Routing-Antworten
            for output_number in range(1, 5):
                response = matrix._readline()

                match = re.fullmatch(
                    rf"MP\s+in([1-4])\s+hdmiout{output_number}",
                    response,
                    re.IGNORECASE,
                )

                if not match:
                    raise RuntimeError(
                        f"Unexpected response: {response}"
                    )

                result[f"out{output_number}_in"] = int(
                    match.group(1)
                )

            # 4 EDID-Antworten
            for input_number in range(1, 5):
                response = matrix._readline()

                match = re.fullmatch(
                    rf"EDID\s+hdmiin{input_number}\s+(\d+)",
                    response,
                    re.IGNORECASE,
                )

                if not match:
                    raise RuntimeError(
                        f"Unexpected response: {response}"
                    )

                result[f"edid_in{input_number}"] = int(
                    match.group(1)
                )

        self.send_json(200, result)


# -------------------------------------------------------------------
# Application
# -------------------------------------------------------------------

def main():
    logger.info(
        "Starting AV Access controller"
    )

    logger.info(
        "Matrix: %s:%s",
        MATRIX_HOST,
        MATRIX_PORT,
    )

    logger.info(
        "HTTP server: %s:%s",
        HTTP_HOST,
        HTTP_PORT,
    )

    # Matrix-Verbindung beim Start herstellen.
    # Falls die Matrix gerade offline ist, darf der Container
    # trotzdem starten.
    try:
        matrix.connect()

    except Exception as exc:
        logger.warning(
            "Initial matrix connection failed: %s",
            exc,
        )

    server = ThreadingHTTPServer(
        (HTTP_HOST, HTTP_PORT),
        RequestHandler,
    )

    # Request-Threads dürfen den Container beim Stoppen
    # nicht künstlich offenhalten.
    server.daemon_threads = True

    def handle_signal(
        signum,
        frame,
    ):
        logger.info(
            "Received signal %s, shutting down",
            signum,
        )

        # KeyboardInterrupt wird im Hauptthread von
        # serve_forever() abgefangen.
        raise KeyboardInterrupt

    signal.signal(
        signal.SIGTERM,
        handle_signal,
    )

    signal.signal(
        signal.SIGINT,
        handle_signal,
    )

    try:
        logger.info(
            "Controller ready"
        )

        server.serve_forever(
            poll_interval=0.2
        )

    except KeyboardInterrupt:
        logger.info(
            "Stopping controller"
        )

    finally:
        server.server_close()
        matrix.close()

        logger.info(
            "Controller stopped"
        )


if __name__ == "__main__":
    main()
