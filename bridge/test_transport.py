"""Real mitmdump + TLS + loopback mock pool; never starts Local Capture."""

import base64
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import http.client
from pathlib import Path
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest


CLIENT_FRAMES = b"\x01\x82mask\x05\x04\x89\x80mask\x80\x83mask\x01\x0d\x1c\x88\x82mask\x6e\x89"
SERVER_FRAMES = b"\x01\x02he\x8a\x00\x80\x03llo\x88\x02\x03\xe8"


class TransportTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.directory = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.directory.cleanup)
        cls.state = Path(cls.directory.name)
        cls.requests = []
        cls.release = threading.Event()

        class MockPool(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def do_POST(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                cls.requests.append((self.path, dict(self.headers), body))
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(b"event: future\ndata: first\n\n")
                self.wfile.flush()
                cls.release.wait(5)
                self.wfile.write(b"data: second\n\n")
                self.close_connection = True

            def do_GET(self):
                cls.requests.append((self.path, dict(self.headers), b""))
                self.send_response(101)
                self.send_header("Connection", "Upgrade")
                self.send_header("Upgrade", "websocket")
                accept = hashlib.sha1((self.headers["Sec-WebSocket-Key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()
                self.send_header("Sec-WebSocket-Accept", base64.b64encode(accept).decode())
                self.end_headers()
                self.wfile.flush()
                cls.received_frames = self.rfile.read(len(CLIENT_FRAMES))
                self.wfile.write(SERVER_FRAMES)
                self.wfile.flush()
                self.close_connection = True

        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), MockPool)
        cls.addClassCleanup(cls.server.server_close)
        cls.addClassCleanup(cls.server.shutdown)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        with socket.socket() as reserve:
            reserve.bind(("127.0.0.1", 0))
            cls.port = reserve.getsockname()[1]
        key = cls.state / "client.key"
        key.write_text("mock-pool-key\n")
        cls.log = open(cls.state / "mitm.log", "w+")
        cls.addClassCleanup(cls.log.close)
        executable = Path(sys.executable).with_name("mitmdump")
        cls.process = subprocess.Popen([
            str(executable), "--mode", "regular", "--listen-host", "127.0.0.1",
            "--listen-port", str(cls.port), "--set", "confdir=" + str(cls.state / "ca"),
            "--set", "flow_detail=0", "--set", "connection_strategy=lazy",
            "--set", "upstream_cert=false", "--set", "websocket=false",
            "--set", "pool_mode=pool", "--set", "pool_key_file=" + str(key),
            "--set", "pool_origin=http://127.0.0.1:" + str(cls.server.server_port),
            "-s", str(Path(__file__).with_name("addon.py")),
        ], stdout=cls.log, stderr=subprocess.STDOUT)
        cls.addClassCleanup(cls.stop_proxy)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if cls.process.poll() is not None:
                cls.log.seek(0)
                raise AssertionError("mitmdump failed: " + cls.log.read())
            try:
                with socket.create_connection(("127.0.0.1", cls.port), timeout=0.1):
                    break
            except OSError:
                time.sleep(0.025)
        else:
            raise AssertionError("mitmdump did not start")
        cls.context = ssl.create_default_context(cafile=str(cls.state / "ca/mitmproxy-ca-cert.pem"))

    @classmethod
    def stop_proxy(cls):
        cls.release.set()
        cls.process.terminate()
        try:
            cls.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            cls.process.kill()
            cls.process.wait(timeout=5)

    def test_tls_streaming_without_body_rewrite_or_waiting_for_eof(self):
        conn = http.client.HTTPSConnection("127.0.0.1", self.port, context=self.context, timeout=5)
        self.addCleanup(conn.close)
        conn.set_tunnel("chatgpt.com", 443)
        body = b' {"unknown_tool":{"type":"future"},"spacing": [1, 2]} \n'
        conn.request("POST", "/backend-api/codex/responses?opaque=a%2Fb", body,
                     {"Authorization": "Bearer original", "Cookie": "original", "Chatgpt-Account-Id": "original"})
        response = conn.getresponse()
        self.assertEqual(response.status, 200)
        self.assertEqual(response.read(len(b"event: future\ndata: first\n\n")), b"event: future\ndata: first\n\n")
        self.release.set()
        self.assertEqual(response.read(), b"data: second\n\n")
        path, headers, received = self.requests[-1]
        self.assertEqual(path, "/backend-api/codex/responses?opaque=a%2Fb")
        self.assertEqual(received, body)
        self.assertEqual(headers["Authorization"], "Bearer mock-pool-key")
        self.assertNotIn("Cookie", headers)
        self.assertNotIn("Chatgpt-Account-Id", headers)

    def test_websocket_frames_control_and_close_are_byte_identical(self):
        conn = http.client.HTTPSConnection("127.0.0.1", self.port, context=self.context, timeout=5)
        self.addCleanup(conn.close)
        conn.set_tunnel("chatgpt.com", 443)
        conn.connect()
        conn.sock.sendall(
            b"GET /backend-api/codex/responses HTTP/1.1\r\nHost: chatgpt.com\r\n"
            b"Authorization: Bearer original\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"
            b"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
        )
        reader = conn.sock.makefile("rb")
        self.addCleanup(reader.close)
        self.assertIn(b"101", reader.readline())
        while reader.readline() != b"\r\n":
            pass
        conn.sock.sendall(CLIENT_FRAMES)
        self.assertEqual(reader.read(len(SERVER_FRAMES)), SERVER_FRAMES)
        self.assertEqual(self.received_frames, CLIENT_FRAMES)
        self.assertEqual(self.requests[-1][1]["Authorization"], "Bearer mock-pool-key")


if __name__ == "__main__":
    unittest.main()
