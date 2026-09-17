#!/usr/bin/env python3
"""Opt-in Mac Local Capture check with one disposable PID and no real credentials.

May prompt macOS to enable mitmproxy's Network Extension. Does not register a CA
in the OS or change any Codex file. Trust is confined to the disposable probe.
"""

import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import subprocess
import sys
import tempfile
import threading


PROBE = r'''
import http.client, json, os, ssl, sys, time
from pathlib import Path
print(os.getpid(), flush=True)
directory = Path(sys.stdin.readline().strip())
deadline = time.monotonic() + 30
certificate = directory / "ca/mitmproxy-ca-cert.pem"
while not certificate.exists() and time.monotonic() < deadline:
    time.sleep(0.1)
if not certificate.exists():
    raise SystemExit("FAIL: test CA was not created")
# This context trusts ONLY the test CA. Without capture, TLS fails before any
# application request is sent to the real official server.
context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
context.load_verify_locations(cafile=str(certificate))
last = "capture not ready"
while time.monotonic() < deadline:
    conn = http.client.HTTPSConnection("chatgpt.com", context=context, timeout=3)
    try:
        conn.request("POST", "/backend-api/codex/responses?capture_probe=true",
                     b'{"probe":"local-capture-only"}',
                     {"Authorization":"Bearer disposable-probe", "Content-Type":"application/json"})
        response = conn.getresponse()
        body = response.read()
        if response.status == 200 and body == b'{"local_capture":"verified"}':
            print("PASS: direct TLS request from probe PID reached the loopback mock pool", flush=True)
            raise SystemExit(0)
        raise SystemExit("FAIL: unexpected mock response (HTTP " + str(response.status) + ")")
    except (OSError, http.client.HTTPException) as exc:
        last = type(exc).__name__
    finally:
        conn.close()
    time.sleep(0.5)
raise SystemExit("FAIL: Local Capture did not intercept the probe (" + last + ")")
'''

# Only used by this disposable probe. Do not let an addon routing defect send
# even the synthetic request to the real service after TLS interception.
PROBE_GUARD = r'''
import logging
from mitmproxy import http
def requestheaders(flow):
    r = flow.request
    if r.host != "127.0.0.1":
        logging.warning("capture probe route mismatch: scheme=%s host=%s authority=%s sni=%s", r.scheme, r.host, r.host_header, flow.client_conn.sni)
        flow.response = http.Response.make(502, b"probe did not route to loopback")
'''


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mitmdump", default=str(Path(sys.executable).with_name("mitmdump")))
    args = parser.parse_args()
    if sys.platform != "darwin":
        parser.error("this check is for macOS")
    if not Path(args.mitmdump).is_file():
        parser.error("pass --mitmdump /path/to/mitmdump")
    with tempfile.TemporaryDirectory(prefix="codex-pool-capture-") as directory:
        state = Path(directory)
        key = state / "client.key"
        key.write_text("disposable-mock-pool-key\n")
        guard = state / "probe_guard.py"
        guard.write_text(PROBE_GUARD)
        received = []

        class MockPool(BaseHTTPRequestHandler):
            def log_message(self, *unused):
                pass

            def do_POST(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                if self.headers.get("Authorization") != "Bearer disposable-mock-pool-key" or body != b'{"probe":"local-capture-only"}':
                    self.send_error(400)
                    return
                received.append(self.path)
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"local_capture":"verified"}')

        server = ThreadingHTTPServer(("127.0.0.1", 0), MockPool)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        probe = subprocess.Popen([sys.executable, "-u", "-c", PROBE], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        bridge = None
        try:
            pid = probe.stdout.readline().strip()
            if not pid.isdigit():
                raise RuntimeError("probe did not report its PID")
            print("Local Capture target: disposable PID " + pid, flush=True)
            with open(state / "mitm.log", "w+") as log:
                bridge = subprocess.Popen([
                    args.mitmdump, "--mode", "local:" + pid,
                    "--set", "confdir=" + str(state / "ca"), "--set", "flow_detail=0",
                    "--set", r"allow_hosts=^chatgpt\.com:443$",
                    "--set", "connection_strategy=lazy", "--set", "upstream_cert=false",
                    "--set", "websocket=false", "--set", "pool_mode=pool",
                    "--set", "pool_origin=http://127.0.0.1:" + str(server.server_port),
                    "--set", "pool_key_file=" + str(key),
                    "-s", str(Path(__file__).with_name("addon.py")),
                    "-s", str(guard),
                ], stdout=log, stderr=subprocess.STDOUT)
                try:
                    output, _ = probe.communicate(str(state) + "\n", timeout=40)
                except subprocess.TimeoutExpired:
                    stop(probe)
                    output = "FAIL: disposable probe timed out"
                stop(bridge)
                print(output.strip(), flush=True)
                if probe.returncode != 0 or not received:
                    log.seek(0)
                    print(log.read().strip(), flush=True)
                    return 1
                print("Mock pool received " + str(len(received)) + " request(s); test processes stopped", flush=True)
                return 0
        finally:
            stop(probe)
            if bridge is not None:
                stop(bridge)
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    raise SystemExit(main())
