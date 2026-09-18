import argparse
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from mitmproxy import http, tcp
from mitmproxy.test import tflow

from addon import Bridge, parse_origin
from run import command


class BridgeTests(unittest.TestCase):
    def bridge(self, mode="pool"):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        key = Path(directory.name) / "key"
        key.write_text("dedicated-client-key\n")
        opts = SimpleNamespace(pool_mode=mode, pool_origin="http://127.0.0.1:18473",
                               pool_key_file=str(key), pool_private_http=False)
        bridge = Bridge()
        with patch("addon.ctx.options", opts, create=True):
            bridge.configure({"pool_mode"})
        return bridge

    def flow(self, path, host="chatgpt.com"):
        flow = tflow.tflow()
        flow.request = http.Request.make("POST", "https://" + host + path,
                                        b' {"future_tool":true,"raw":[1,2]} \n',
                                        {"Authorization": "Bearer original", "Cookie": "secret",
                                         "Chatgpt-Account-Id": "original", "X-OpenAI-Actor": "original",
                                         "X-Future-Feature": "preserve"})
        return flow

    def test_observe_keeps_original_destination_auth_and_bytes(self):
        bridge = self.bridge("observe")
        flow = self.flow("/backend-api/codex/responses?opaque=a%2Fb")
        before = flow.request.get_state()
        bridge.requestheaders(flow)
        self.assertEqual(before, flow.request.get_state())
        self.assertTrue(flow.request.stream)

    def test_all_target_host_paths_keep_path_query_method_and_body(self):
        bridge = self.bridge()
        for path in ("/backend-api/codex/responses", "/backend-api/codex/images/generations",
                     "/backend-api/codex/images/edits", "/backend-api/codex/responses/compact",
                     "/backend-api/wham/usage", "/backend-api/files/file-id/download",
                     "/backend-api/codex/future-route", "/future-api/%66eature/%2fresource",
                     "/backend-api/me", "/backend-api/connectors", "/anything-unknown"):
            with self.subTest(path=path):
                flow = self.flow(path + "?account_id=opaque&token=opaque&x=%zz;unchanged")
                flow.request.method = "FUTURE"
                body = flow.request.raw_content
                original_path = flow.request.path
                bridge.requestheaders(flow)
                self.assertEqual((flow.request.scheme, flow.request.host, flow.request.port),
                                 ("http", "127.0.0.1", 18473))
                self.assertEqual(flow.request.path, original_path)
                self.assertEqual(flow.request.method, "FUTURE")
                self.assertEqual(flow.request.raw_content, body)
                self.assertEqual(flow.request.headers["Authorization"], "Bearer dedicated-client-key")
                for name in ("Cookie", "Chatgpt-Account-Id", "X-OpenAI-Actor"):
                    self.assertNotIn(name, flow.request.headers)
                self.assertEqual(flow.request.headers["X-Future-Feature"], "preserve")
                self.assertTrue(flow.request.stream)

    def test_other_host_requests_are_untouched(self):
        bridge = self.bridge()
        for host, path in (("auth.openai.com", "/oauth/token"), ("example.com", "/backend-api/codex/responses")):
            flow = self.flow(path, host)
            before = flow.request.get_state()
            bridge.requestheaders(flow)
            self.assertEqual(flow.request.get_state(), before)

    def test_local_capture_uses_host_or_http2_authority_with_destination_ip(self):
        bridge = self.bridge()
        for version in (b"HTTP/1.1", b"HTTP/2.0"):
            flow = self.flow("/backend-api/codex/responses?opaque=a%2Fb")
            flow.request.host = "192.0.2.1"
            flow.request.data.http_version = version
            flow.request.host_header = "chatgpt.com"
            before = flow.request.raw_content
            bridge.requestheaders(flow)
            self.assertEqual(flow.request.host, "127.0.0.1")
            self.assertEqual(flow.request.port, 18473)
            self.assertEqual(flow.request.path, "/backend-api/codex/responses?opaque=a%2Fb")
            self.assertEqual(flow.request.raw_content, before)
            self.assertEqual(flow.request.headers["Authorization"], "Bearer dedicated-client-key")

    def test_response_streams_errors_and_opaque_bytes(self):
        bridge = self.bridge()
        flow = self.flow("/backend-api/codex/responses")
        bridge.requestheaders(flow)
        for status in (200, 302, 401, 429, 500):
            flow.response = http.Response.make(status, b"event: future\ndata: opaque\n\n",
                                               {"Content-Type": "text/event-stream", "Set-Cookie": "original"})
            bridge.responseheaders(flow)
            self.assertTrue(flow.response.stream)
            self.assertEqual(flow.response.status_code, status)
            self.assertEqual(flow.response.raw_content, b"event: future\ndata: opaque\n\n")
            self.assertNotIn("Set-Cookie", flow.response.headers)

    def test_websocket_tcp_history_does_not_rewrite_current_chunk(self):
        bridge = self.bridge()
        flow = tflow.ttcpflow()
        old = tcp.TCPMessage(True, b"old")
        current = tcp.TCPMessage(True, b"\x89\x00\x81\x80opaque-masked-frame")
        flow.messages = [old, current]
        bridge.tcp_message(flow)
        self.assertEqual(flow.messages, [current])
        self.assertEqual(current.content, b"\x89\x00\x81\x80opaque-masked-frame")

    def test_origin_does_not_allow_arbitrary_urls_or_cleartext_remote_by_default(self):
        for bad in ("file:///tmp/secret", "https://a/v1", "https://u:p@a", "https://a?token=secret",
                    "https://a/#fragment", "http://192.0.2.10:18473"):
            with self.subTest(origin=bad), self.assertRaises(ValueError):
                parse_origin(bad)
        self.assertEqual(parse_origin("https://pool.example:443")[1], 443)
        self.assertEqual(parse_origin("http://192.0.2.10:18473", True)[1], 18473)

    def test_launcher_changes_only_bridge_arguments(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            config = root / "pool.json"
            config.write_text(json.dumps({"state_dir": "state", "listen": "127.0.0.1:18473"}))
            args = argparse.Namespace(mode="ca", config=str(config), capture="12345", origin=None,
                                      private_http=False, mitmdump="/test/mitmdump", exclude_codex_app=None)
            self.assertNotIn("--mode", command(args))
            (root / "state/ca/mitmproxy-ca-cert.pem").touch()
            args.mode = "pool"
            cmd = command(args)
            self.assertIn("local:12345", cmd)
            self.assertIn("websocket=false", cmd)
            self.assertIn("pool_origin=http://127.0.0.1:18473", cmd)
            self.assertIn("pool_key_file=" + str(root / "state/client.key"), cmd)
            self.assertNotIn("ssl_insecure=true", cmd)
            self.assertEqual(json.loads(config.read_text())["state_dir"], "state")

    def test_launcher_reads_private_bridge_config_and_cli_origin_override(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            config = root / "bridge.json"
            for origin, private_http in (("https://pool.example:18473", False),
                                         ("http://192.0.2.10:18473", True)):
                with self.subTest(origin=origin):
                    config.write_text(json.dumps({"state_dir": "state", "origin": origin,
                                                  "private_http": private_http}))
                    before = config.read_bytes()
                    args = argparse.Namespace(mode="ca", config=str(config), capture="12345", origin=None,
                                              private_http=False, mitmdump="/test/mitmdump", exclude_codex_app=None)
                    command(args)
                    (root / "state/ca/mitmproxy-ca-cert.pem").touch()
                    args.mode = "pool"
                    cmd = command(args)
                    self.assertIn("pool_origin=" + origin, cmd)
                    self.assertIn("pool_private_http=" + str(private_http).lower(), cmd)
                    self.assertIn("pool_key_file=" + str(root / "state/client.key"), cmd)
                    args.origin = "https://override.example"
                    self.assertIn("pool_origin=https://override.example", command(args))
                    args.mode = "observe"
                    self.assertFalse(any(value.startswith("pool_origin=") for value in command(args)))
                    self.assertEqual(config.read_bytes(), before)

    def test_launcher_can_exclude_app_bundles_without_changing_relay(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            config = root / "bridge.json"
            ca = root / "state/ca"
            ca.mkdir(parents=True)
            (ca / "mitmproxy-ca-cert.pem").touch()
            for mode in ("pool", "observe"):
                for configured in (None, False, True):
                    data = {"state_dir": "state", "origin": "https://pool.example"}
                    if configured is not None:
                        data["exclude_codex_app"] = configured
                    config.write_text(json.dumps(data))
                    before = config.read_bytes()
                    for cli in (None, False, True):
                        for capture in ("codex,Codex", "12345", "codex,!other"):
                            with self.subTest(mode=mode, configured=configured, cli=cli, capture=capture):
                                args = argparse.Namespace(mode=mode, config=str(config), capture=capture,
                                                          origin=None, private_http=False,
                                                          mitmdump="/test/mitmdump", exclude_codex_app=False)
                                baseline = command(args)
                                args.exclude_codex_app = cli
                                actual = command(args)
                                enabled = configured if cli is None else cli
                                expected = baseline.copy()
                                if enabled:
                                    expected[expected.index("--mode") + 1] += ",!/Codex.app/,!/ChatGPT.app/"
                                self.assertEqual(actual, expected)
                                self.assertEqual(config.read_bytes(), before)


if __name__ == "__main__":
    unittest.main()
