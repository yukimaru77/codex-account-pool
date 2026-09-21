"""Prepared JSONL conversion tests; no Codex files or real accounts are used."""

import importlib.util
import io
import json
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import threading
from types import SimpleNamespace
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


SCRIPT = Path(__file__).with_name("compact-jsonl.py")
SPEC = importlib.util.spec_from_file_location("compact_jsonl", SCRIPT)
compact = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(compact)


def event(kind, **fields):
    return ("event: " + kind + "\ndata: " + json.dumps({"type": kind, **fields}) + "\n\n").encode()


ITEM = {"type": "compaction", "id": "cmp_fixture", "encrypted_content": "opaque-blob",
        "future_field": {"keep": [True, "unchanged"]}}
SUCCESS = (b": heartbeat\n\n" + event("future.event", unknown=True)
           + event("response.output_item.done", item=ITEM)
           + event("response.completed", response={"status": "completed", "output": []}))


class PreparedJSONLTests(unittest.TestCase):
    def test_input_lines_are_kept_verbatim_without_rollout_processing(self):
        lines = [b'{"type":"message", "role":"user", "content":"hello"}',
                 b'{"type":"future_tool", "number":1.234567890123456789, "opaque":true}',
                 b'{"type":"compacted", "payload":{"replacement_history":[]}}',
                 b'{"type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":9}}']
        original = b'\n\n'.join(lines) + b'\n'
        body = compact.request_body(io.BytesIO(original), "exact-model", "exact instructions")
        for line in lines:
            self.assertIn(line, body)
        parsed = json.loads(body)
        self.assertEqual(parsed["model"], "exact-model")
        self.assertEqual(parsed["instructions"], "exact instructions")
        self.assertEqual(parsed["input"][:-1], [json.loads(line) for line in lines])
        self.assertEqual(parsed["input"][-1], {"type": "compaction_trigger"})

    def test_invalid_json_reports_only_line_number(self):
        with self.assertRaisesRegex(ValueError, "^invalid JSON on line 2$"):
            compact.request_body(io.BytesIO(b'{}\nprivate invalid content\n'), "m", "i")

    def test_multiline_sse_preserves_unknown_item_fields(self):
        data = (b'data: {"type":"response.output_item.done",\r\n'
                + b'data: "item":' + json.dumps(ITEM).encode() + b'}\r\n\r\n'
                + event("response.completed"))
        self.assertEqual(compact.compaction_item(io.BytesIO(data)), ITEM)

    def test_no_blob_no_completion_and_failure_events_fail(self):
        streams = [event("response.completed"), event("response.output_item.done", item=ITEM),
                   event("response.output_item.done", item={"type":"message", "content":"plain summary"}) + event("response.completed"),
                   event("response.output_item.done", item={"type":"compaction", "encrypted_content":""}) + event("response.completed"),
                   event("response.output_item.done", item=ITEM) + event("response.failed"),
                   event("response.incomplete"), event("error"), b'data: [DONE]\n\n']
        for data in streams:
            with self.subTest(data=data), self.assertRaises(ValueError):
                compact.compaction_item(io.BytesIO(data))

    def test_remote_http_uses_existing_private_tunnel_opt_in(self):
        with self.assertRaises(ValueError):
            compact.endpoint("http://192.0.2.10:18473", False)
        self.assertEqual(compact.endpoint("http://192.0.2.10:18473", True),
                         "http://192.0.2.10:18473/_pool/rr/responses")
        self.assertEqual(compact.endpoint("https://pool.example/", False),
                         "https://pool.example/_pool/rr/responses")

    def test_shared_config_changes_take_effect_and_explicit_values_win(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            config = root / "bridge.json"
            args = SimpleNamespace(pool_config=str(config), origin=None, key_file=None, private_http=False)
            settings = {"origin": "https://old.example", "state_dir": "pool state"}
            config.write_text(json.dumps(settings))
            self.assertEqual(compact.connection(args),
                             ("https://old.example/_pool/rr/responses", root / "pool state/client.key"))
            settings.update(origin="http://new.example:18473", private_http=True, key_file="keys/client.key")
            config.write_text(json.dumps(settings))
            self.assertEqual(compact.connection(args),
                             ("http://new.example:18473/_pool/rr/responses", root / "keys/client.key"))
            args.origin, args.key_file = "https://override.example", str(root / "override.key")
            self.assertEqual(compact.connection(args),
                             ("https://override.example/_pool/rr/responses", root / "override.key"))


class CommandTests(unittest.TestCase):
    def run_command(self, response=SUCCESS, status=200, stdin=False, same_output=False, invalid=False, shared=False):
        requests = []

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args): pass

            def do_POST(self):
                requests.append((self.path, self.headers.get("Authorization"),
                                 self.rfile.read(int(self.headers["Content-Length"]))))
                self.send_response(status)
                # Real upstream also sends SSE with a text/plain content type.
                self.send_header("Content-Type", "text/plain")
                self.send_header("Content-Length", str(len(response)))
                if status == 307:
                    self.send_header("Location", "/must-not-follow")
                self.end_headers()
                try:
                    for start in range(0, len(response), 7):
                        self.wfile.write(response[start:start+7])
                        self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    # Error/redirect responses are deliberately closed unread.
                    pass

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "prepared.jsonl"
            original = b'{"role":"user","content":"marker","unknown":37}\n'
            if invalid: original = b'private invalid input\n'
            source.write_bytes(original)
            key = root / "client.key"
            key.write_text("private-test-key\n")
            output = source if same_output else root / "compact.json"
            if not same_output: output.write_text("previous output")
            server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                command = [sys.executable, str(SCRIPT), "-" if stdin else str(source),
                           "--model", "test-model", "--output", str(output)]
                origin = f"http://127.0.0.1:{server.server_port}"
                if shared:
                    config = root / "bridge.json"
                    config.write_text(json.dumps({"origin": origin, "key_file": "client.key"}))
                    command += ["--pool-config", str(config)]
                else:
                    command += ["--origin", origin, "--key-file", str(key)]
                result = subprocess.run(command, input=original if stdin else None, capture_output=True, timeout=10)
            finally:
                server.shutdown()
                server.server_close()
                thread.join()
            self.assertEqual(source.read_bytes(), original)
            self.assertEqual(key.read_text(), "private-test-key\n")
            self.assertNotIn(b"private-test-key", result.stdout + result.stderr)
            self.assertNotIn(b"private invalid input", result.stdout + result.stderr)
            if status == 200 and response == SUCCESS and not same_output and not invalid:
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(json.loads(output.read_text()), ITEM)
                self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
                self.assertEqual(len(requests), 1)
                path, authorization, body = requests[0]
                self.assertEqual(path, "/_pool/rr/responses")
                self.assertEqual(authorization, "Bearer private-test-key")
                self.assertIn(original.strip(), body)
            else:
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(output.read_bytes(), original if same_output else b"previous output")
                self.assertEqual(len(requests), 0 if same_output or invalid else 1)
            self.assertEqual(list(root.glob(".compact-*")), [])

    def test_success_file(self): self.run_command()
    def test_success_stdin(self): self.run_command(stdin=True)
    def test_shared_config_sends_to_its_origin_with_its_key(self): self.run_command(shared=True)
    def test_input_output_same_file_is_untouched(self): self.run_command(same_output=True)
    def test_invalid_input_does_not_send(self): self.run_command(invalid=True)
    def test_no_blob_does_not_replace_output(self): self.run_command(response=event("response.completed"))
    def test_truncated_stream_does_not_replace_output(self): self.run_command(response=event("response.output_item.done", item=ITEM))
    def test_http_error_is_not_retried(self): self.run_command(status=429, response=b'private-test-key')
    def test_redirect_is_not_followed(self): self.run_command(status=307)


if __name__ == "__main__":
    unittest.main()
