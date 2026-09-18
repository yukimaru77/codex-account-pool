#!/usr/bin/env python3
"""Start only the bridge process. Never configure or launch Codex."""

import argparse
import json
import os
from pathlib import Path
import shutil


def command(args):
    config_path = Path(args.config).expanduser().resolve()
    config = json.loads(config_path.read_text())
    state = Path(config["state_dir"])
    if not state.is_absolute():
        state = config_path.parent / state
    ca = state / "ca"
    ca.mkdir(mode=0o700, parents=True, exist_ok=True)
    ca.chmod(0o700)
    executable = args.mitmdump or shutil.which("mitmdump")
    if not executable:
        raise ValueError("install mitmproxy or pass --mitmdump /path/to/mitmdump")
    cmd = [executable, "--listen-host", "127.0.0.1", "--listen-port", "18081",
           "--set", "confdir=" + str(ca), "--set", "flow_detail=0"]
    if args.mode == "ca":
        return cmd
    if not (ca / "mitmproxy-ca-cert.pem").is_file():
        raise ValueError("first run 'bridge/run.py ca' and trust that dedicated CA as documented")
    if not args.capture or args.capture.startswith("!"):
        raise ValueError("--capture must name only the intended Codex process(es) or PID(s)")
    capture = args.capture
    exclude_app = args.exclude_codex_app
    if exclude_app is None:
        exclude_app = config.get("exclude_codex_app", False)
    if exclude_app:
        # macOS Local Capture matches the full executable path. App helpers
        # also use the name "codex", so excluding just "Codex" is insufficient.
        capture += ",!/Codex.app/,!/ChatGPT.app/"
    cmd += ["--mode", "local:" + capture,
            "--set", r"allow_hosts=^chatgpt\.com:443$",
            "--set", "connection_strategy=lazy", "--set", "upstream_cert=false",
            "--set", "websocket=false", "--set", "rawtcp=true",
            "--set", "store_streamed_bodies=false",
            "--set", "pool_mode=" + args.mode,
            "-s", str(Path(__file__).with_name("addon.py"))]
    if args.mode == "pool":
        origin = args.origin or config.get("origin")
        if not origin:
            scheme = "https" if config.get("tls_cert") else "http"
            origin = scheme + "://" + config["listen"]
        cmd += ["--set", "pool_origin=" + origin,
                "--set", "pool_key_file=" + str(state / "client.key"),
                "--set", "pool_private_http=" + str(args.private_http or config.get("private_http", False)).lower()]
    return cmd


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["ca", "observe", "pool"])
    parser.add_argument("--config", default="pool.json")
    parser.add_argument("--capture", default="codex,Codex")
    parser.add_argument("--exclude-codex-app", action=argparse.BooleanOptionalAction, default=None,
                        help="leave Codex.app/ChatGPT.app bundle processes on their direct connection (Mac); also configurable as exclude_codex_app")
    parser.add_argument("--origin", help="pool origin; defaults to config origin or listen address")
    parser.add_argument("--private-http", action="store_true", help="allow HTTP over a verified private tunnel; also configurable in bridge.json")
    parser.add_argument("--mitmdump")
    args = parser.parse_args()
    try:
        cmd = command(args)
    except (OSError, ValueError, KeyError) as exc:
        parser.error(str(exc))
    os.execvp(cmd[0], cmd)


if __name__ == "__main__":
    main()
