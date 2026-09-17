import re
import socket
import threading
import logging

logger = logging.getLogger("av-access-controller")


class MatrixConnection:
    def __init__(self, host: str, port: int, timeout: float):
        self.host = host
        self.port = port
        self.timeout = timeout

        self.socket: socket.socket | None = None
        self.reader = None

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
        if self.reader:
            try:
                self.reader.close()
            except Exception:
                pass

        if self.socket:
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
        expected_pattern: str | None = None,
    ) -> str:
        self.ensure_connected()

        payload = f"{command}\r\n".encode("ascii")

        logger.debug("TX: %s", command)

        self.socket.sendall(payload)

        while True:
            response = self._readline()

            if expected_pattern is None:
                return response

            if re.fullmatch(
                expected_pattern,
                response,
                re.IGNORECASE,
            ):
                return response

            logger.debug(
                "Ignoring unexpected response: %s "
                "(expected %s)",
                response,
                expected_pattern,
            )

    def command(
        self,
        command: str,
        expected_pattern: str | None = None,
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

                self.connect()

                return self._send(
                    command,
                    expected_pattern,
                )

    def switch(
        self,
        input_number: int,
        output_number: int,
    ) -> str:

        self._validate_input(input_number)
        self._validate_output(output_number)

        return self.command(
            f"SET SW hdmiin{input_number} hdmiout{output_number}",
        )

    def set_edid(
        self,
        input_number: int,
        edid: int,
    ) -> str:

        self._validate_input(input_number)

        if not 1 <= edid <= 15:
            raise ValueError("EDID must be between 1 and 15")

        return self.command(
            f"SET EDID hdmiin{input_number} {edid}",
        )

    def get_output(
        self,
        output_number: int,
    ) -> str:

        self._validate_output(output_number)

        return self.command(
            f"GET MP hdmiout{output_number}",
            rf"MP\s+in[1-4]\s+hdmiout{output_number}",
        )

    def get_edid(
        self,
        input_number: int,
    ) -> str:

        self._validate_input(input_number)

        return self.command(
            f"GET EDID hdmiin{input_number}",
            rf"EDID\s+hdmiin{input_number}\s+\d+",
        )

    @staticmethod
    def _validate_input(number: int):
        if not 1 <= number <= 4:
            raise ValueError("Input must be between 1 and 4")

    @staticmethod
    def _validate_output(number: int):
        if not 1 <= number <= 4:
            raise ValueError("Output must be between 1 and 4")
