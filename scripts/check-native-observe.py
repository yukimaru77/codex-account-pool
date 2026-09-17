#!/usr/bin/env python3
"""Compare an installed Codex using its current login/config with a PID-only bridge.

This sends a short real request using the existing Codex login. It does not set
Codex environment variables or provider/model/tool options. Private test output
is stored under the pool state directory. A new ephemeral Codex turn is used.
"""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time


def snapshot(paths):
    return {str(path): hashlib.sha256(path.read_bytes()).hexdigest() for path in paths}


def stop(process):
    if process and process.poll() is None:
        os.killpg(process.pid, signal.SIGTERM)
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["direct", "observe", "pool"])
    parser.add_argument("--config", default="pool.json")
    parser.add_argument("--codex", default=shutil.which("codex"))
    parser.add_argument("--mitmdump", default=str(Path(__file__).resolve().parents[1] / "bridge/.venv/bin/mitmdump"))
    parser.add_argument("--origin", help="remote pool origin, passed only to the bridge")
    parser.add_argument("--private-http", action="store_true")
    parser.add_argument("--hash-file", action="append", default=[])
    parser.add_argument("--timeout", type=int, default=180)
    args = parser.parse_args()
    config_path = Path(args.config).resolve()
    cfg = json.loads(config_path.read_text())
    state = Path(cfg["state_dir"])
    if not state.is_absolute():
        state = config_path.parent / state
    checks = state / "checks"
    checks.mkdir(mode=0o700, parents=True, exist_ok=True)
    result_dir = Path(tempfile.mkdtemp(prefix=args.mode + "-", dir=checks))
    work = result_dir / "work"
    work.mkdir(mode=0o700)
    codex_config = Path(os.environ.get("CODEX_HOME", str(Path.home() / ".codex"))) / "config.toml"
    paths = [Path(args.codex), codex_config] + [Path(p) for p in args.hash_file]
    before = snapshot(paths)
    marker = "NATIVE_RELAY_CHECK_OK"
    answer = result_dir / "answer.txt"
    answer.touch(mode=0o600)
    native = bridge = None
    bridge_log = result_dir / "bridge.log"
    print("Native check:", args.mode, "results:", result_dir, flush=True)
    try:
        with open(result_dir / "events.jsonl", "w") as stdout, open(result_dir / "stderr.log", "w") as stderr, open(bridge_log, "w") as proxy_output:
            for stream in (stdout, stderr, proxy_output):
                os.fchmod(stream.fileno(), 0o600)
            native = subprocess.Popen([
                "/bin/bash", "-c", 'read -r pool_ready; exec "$@"', "native-check",
                args.codex, "exec", "--json", "--ephemeral", "--skip-git-repo-check",
                "--output-last-message", str(answer),
                "Reply with exactly " + marker + ". Do not use tools or access files.",
            ], cwd=work, stdin=subprocess.PIPE, stdout=stdout, stderr=stderr, text=True, start_new_session=True)
            if args.mode != "direct":
                module_path = Path(__file__).resolve().parents[1] / "bridge/run.py"
                spec = importlib.util.spec_from_file_location("pool_bridge_runner", module_path)
                runner = importlib.util.module_from_spec(spec)
                spec.loader.exec_module(runner)
                bridge_args = argparse.Namespace(mode=args.mode, config=str(config_path), capture=str(native.pid),
                                                 origin=args.origin, private_http=args.private_http, mitmdump=args.mitmdump)
                bridge = subprocess.Popen(runner.command(bridge_args), stdout=proxy_output, stderr=subprocess.STDOUT,
                                          env=dict(os.environ, PYTHONUNBUFFERED="1"), start_new_session=True)
                deadline = time.monotonic() + 30
                while time.monotonic() < deadline:
                    if bridge.poll() is not None:
                        raise RuntimeError("bridge exited during startup; inspect private bridge.log")
                    if "Local redirector started." in bridge_log.read_text():
                        break
                    time.sleep(0.1)
                else:
                    raise RuntimeError("Local Capture did not become ready; inspect private bridge.log")
            print("Codex test PID:", native.pid, flush=True)
            native.stdin.write("ready\n")
            native.stdin.close()
            try:
                exit_code = native.wait(timeout=args.timeout)
            except subprocess.TimeoutExpired:
                stop(native)
                exit_code = native.returncode
            stop(bridge)
        text = answer.read_text().strip() if answer.exists() else ""
        after = snapshot(paths)
        bridge_required = args.mode != "direct"
        intercepted = bridge_required and "pool bridge: " + args.mode in bridge_log.read_text()
        report = {"mode": args.mode, "exit_code": exit_code, "expected_response": text == marker,
                  "bridge_required": bridge_required,
                  "bridge_observed_native_request": intercepted, "hashes_unchanged": before == after,
                  "before_sha256": before, "after_sha256": after}
        report_path = result_dir / "result.json"
        report_path.write_text(json.dumps(report, indent=2) + "\n")
        report_path.chmod(0o600)
        summary = {key: value for key, value in report.items() if not key.endswith("sha256")}
        print(json.dumps(summary), flush=True)
        print("Report:", report_path, flush=True)
        return 0 if exit_code == 0 and text == marker and before == after and (not bridge_required or intercepted) else 1
    finally:
        stop(native)
        stop(bridge)


if __name__ == "__main__":
    raise SystemExit(main())
