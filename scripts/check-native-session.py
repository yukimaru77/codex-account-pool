#!/usr/bin/env python3
"""Exercise an installed Codex's tools, compaction, and cancellation with its current config."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import queue
import shutil
import subprocess
import tempfile
import threading
import time


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["direct", "pool"])
    parser.add_argument("--config", default="pool.json")
    parser.add_argument("--origin")
    parser.add_argument("--private-http", action="store_true")
    parser.add_argument("--codex", default=shutil.which("codex"))
    parser.add_argument("--scenario", choices=["tools", "compact", "interrupt"], required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    helpers = load_module("native_check", root / "scripts/check-native-observe.py")
    runner = load_module("bridge_runner", root / "bridge/run.py")
    cfg_path = Path(args.config).resolve()
    cfg = json.loads(cfg_path.read_text())
    state = Path(cfg["state_dir"])
    if not state.is_absolute():
        state = cfg_path.parent / state
    (state / "checks").mkdir(mode=0o700, parents=True, exist_ok=True)
    result_dir = Path(tempfile.mkdtemp(prefix=f"session-{args.mode}-{args.scenario}-", dir=state / "checks"))
    work = result_dir / "work"
    work.mkdir(mode=0o700)
    config = Path(os.environ.get("CODEX_HOME", str(Path.home() / ".codex"))) / "config.toml"
    paths = [Path(args.codex), config, Path("/opt/homebrew/bin/codex")]
    before = helpers.snapshot(paths)
    native = bridge = None
    events = queue.Queue()
    report = {"mode": args.mode, "scenario": args.scenario, "checks": []}
    print("Native session results:", result_dir, flush=True)
    logs = []
    try:
        for name in ("stderr.log", "bridge.log", "events.jsonl"):
            stream = (result_dir / name).open("w")
            os.fchmod(stream.fileno(), 0o600)
            logs.append(stream)
        stderr, proxy_log, event_log = logs
        native = subprocess.Popen(["/bin/bash", "-c", 'read -r pool_ready; exec "$@"', "native-session",
                                   args.codex, "app-server"], cwd=work, stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=stderr, text=True, start_new_session=True)
        if args.mode == "pool":
            bridge_args = argparse.Namespace(mode="pool", config=str(cfg_path), capture=str(native.pid),
                                             origin=args.origin, private_http=args.private_http,
                                             mitmdump=str(root / "bridge/.venv/bin/mitmdump"),
                                             exclude_codex_app=None)
            bridge = subprocess.Popen(runner.command(bridge_args), stdout=proxy_log, stderr=subprocess.STDOUT,
                                      env=dict(os.environ, PYTHONUNBUFFERED="1"), start_new_session=True)
            deadline = time.monotonic() + 30
            while "Local redirector started." not in (result_dir / "bridge.log").read_text():
                if bridge.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError("bridge startup failed")
                time.sleep(0.1)
        native.stdin.write("ready\n")
        native.stdin.flush()

        def read_events():
            for line in native.stdout:
                event_log.write(line)
                event_log.flush()
                try:
                    events.put(json.loads(line))
                except ValueError:
                    pass
            events.put({"process_exited": True})

        reader = threading.Thread(target=read_events, daemon=True)
        reader.start()
        next_id = 0
        seen = []

        def send(method, params, request=True):
            nonlocal next_id
            obj = {"method": method, "params": params}
            if request:
                next_id += 1
                obj["id"] = next_id
            native.stdin.write(json.dumps(obj) + "\n")
            native.stdin.flush()
            return obj.get("id")

        def wait(predicate, timeout=180, history_start=None):
            if history_start is not None:
                for event in seen[history_start:]:
                    if predicate(event):
                        return event
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                try:
                    event = events.get(timeout=min(1, max(0.01, deadline-time.monotonic())))
                except queue.Empty:
                    continue
                if event.get("process_exited"):
                    raise RuntimeError("Codex process exited")
                seen.append(event)
                if "id" in event and "method" in event:
                    raise RuntimeError("unexpected interactive server request; inspect private events")
                if predicate(event):
                    return event
            raise RuntimeError("timed out waiting for Codex event")

        def request(method, params):
            rid = send(method, params)
            result = wait(lambda e: e.get("id") == rid)
            if "error" in result:
                raise RuntimeError(method + " returned an error; inspect private events")
            return result["result"]

        request("initialize", {"clientInfo": {"name": "codex_cli_rs", "version": "0.153.4"},
                               "capabilities": {"experimentalApi": True}})
        send("initialized", {}, request=False)
        thread = request("thread/start", {"cwd": str(work), "ephemeral": True})["thread"]["id"]
        report["thread_id"] = thread

        def turn(prompt, expected):
            start = len(seen)
            result = request("turn/start", {"threadId": thread, "input": [{"type": "text", "text": prompt}]})
            turn_id = result["turn"]["id"]
            done = wait(lambda e: e.get("method") == "turn/completed" and e["params"]["turn"]["id"] == turn_id, history_start=start)
            messages = [e["params"]["item"].get("text", "") for e in seen[start:]
                        if e.get("method") == "item/completed" and e["params"]["item"].get("type") == "agentMessage"]
            ok = done["params"]["turn"]["status"] == "completed" and any(expected in m for m in messages)
            report["checks"].append({"name": expected, "ok": ok, "status": done["params"]["turn"]["status"]})
            print(json.dumps(report["checks"][-1]), flush=True)
            return ok

        if args.scenario == "tools":
            turn("Using the shell tool, create relay-marker.txt in the current directory containing exactly POOL_TOOL_OK. Read it back with the tool, then reply exactly POOL_TOOL_OK. Do not change other files.", "POOL_TOOL_OK")
            marker = work / "relay-marker.txt"
            report["checks"].append({"name": "tool_file", "ok": marker.is_file() and marker.read_text().strip() == "POOL_TOOL_OK"})
        elif args.scenario == "compact":
            turn("Remember the marker SAPPHIRE_POOL_724. Reply exactly SAVED. Do not use tools.", "SAVED")
            start = len(seen)
            request("thread/compact/start", {"threadId": thread})
            done = wait(lambda e: e.get("method") == "turn/completed", history_start=start)
            compacted = any(e.get("method") == "item/completed" and e["params"]["item"].get("type") == "contextCompaction" for e in seen[start:])
            report["checks"].append({"name": "compaction", "ok": compacted and done["params"]["turn"]["status"] == "completed", "status": done["params"]["turn"]["status"]})
            print(json.dumps(report["checks"][-1]), flush=True)
            turn("Reply with exactly the marker I asked you to remember. Do not use tools.", "SAPPHIRE_POOL_724")
        else:
            start = len(seen)
            result = request("turn/start", {"threadId": thread, "input": [{"type": "text", "text": "Explain the numbers from 1 through 1000 in detail. Do not use tools."}]})
            turn_id = result["turn"]["id"]
            wait(lambda e: e.get("method") == "item/agentMessage/delta" or (
                e.get("method") == "item/completed" and e.get("params", {}).get("item", {}).get("type") == "reasoning"), history_start=start)
            report["checks"].append({"name": "stream_started_before_interrupt", "ok": True})
            request("turn/interrupt", {"threadId": thread, "turnId": turn_id})
            done = wait(lambda e: e.get("method") == "turn/completed" and e["params"]["turn"]["id"] == turn_id, history_start=start)
            report["checks"].append({"name": "interrupt", "ok": done["params"]["turn"]["status"] == "interrupted"})
            turn("Reply exactly POOL_RECOVERY_OK. Do not use tools.", "POOL_RECOVERY_OK")
    except Exception as exc:
        report["error"] = str(exc)
    finally:
        helpers.stop(native)
        helpers.stop(bridge)
        if "reader" in locals():
            reader.join(timeout=3)
        for stream in logs:
            stream.close()
        report["hashes_unchanged"] = before == helpers.snapshot(paths)
        report["before_sha256"] = before
        report["after_sha256"] = helpers.snapshot(paths)
        report["bridge_observed_native_request"] = args.mode == "pool" and "pool bridge: pool" in (result_dir / "bridge.log").read_text()
        report["ok"] = bool(report["checks"]) and all(c["ok"] for c in report["checks"]) and "error" not in report and report["hashes_unchanged"] and (args.mode == "direct" or report["bridge_observed_native_request"])
        path = result_dir / "result.json"
        path.write_text(json.dumps(report, indent=2)+"\n")
        path.chmod(0o600)
        print(json.dumps({k:v for k,v in report.items() if not k.endswith("sha256")}), flush=True)
        print("Report:", path, flush=True)
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
