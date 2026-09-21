#!/usr/bin/env python3
"""Run codex exec or kb NAME codex through the pool's explicit round-robin endpoints."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import sys


ROOT = Path(__file__).resolve().parent.parent


def command(config_path, argv):
    executable = Path(argv[0]).name if argv else None
    is_codex = executable == "codex" and argv[1:2] == ["exec"]
    is_kb = executable == "kb" and len(argv) >= 3 and (
        (argv[1] == "codex" and not argv[2].startswith("-"))
        or ("codex" in argv[2:] and any(
            not arg.startswith("-") for arg in argv[1:argv.index("codex", 2)]
        ))
    )
    if not is_codex and not is_kb:
        raise ValueError("usage: pool-rr [--config bridge.json] {codex exec | kb NAME [KB-options] codex} ...")
    config_path = Path(config_path).expanduser().resolve()
    config = json.loads(config_path.read_text())
    spec = importlib.util.spec_from_file_location("compact_jsonl", ROOT / "scripts/compact-jsonl.py")
    compact = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(compact)
    private_http = config.get("private_http", False)
    base = compact.endpoint(config["origin"], private_http).removesuffix("/responses")
    state = Path(config.get("state_dir", "state")).expanduser()
    if not state.is_absolute():
        state = config_path.parent / state
    key_file = (state / "client.key").resolve()
    key = key_file.read_text().strip()
    if not key or any(c.isspace() for c in key):
        raise ValueError("invalid pool client key")
    provider = {
        "name": "Account Pool Round Robin",
        "base_url": base,
        "env_key": "CODEX_POOL_RR_KEY",
        "wire_api": "responses",
        "requires_openai_auth": False,
        # SSE makes each inference a separate selection, including tool turns.
        "supports_websockets": False,
        "request_max_retries": 0,
        "stream_max_retries": 0,
    }
    table = "{ " + ", ".join(k + " = " + json.dumps(v) for k, v in provider.items()) + " }"
    catalog = Path(config.get("rr_model_catalog", str(Path(os.environ.get("CODEX_HOME", Path.home() / ".codex")) / "models_cache.json"))).expanduser().resolve()
    if not catalog.is_file():
        raise ValueError("model catalog missing: run normal codex once or set rr_model_catalog in bridge.json")
    overrides = ['model_provider="pool_rr"', "model_providers.pool_rr=" + table,
                 "model_catalog_json=" + json.dumps(str(catalog))]
    env = dict(os.environ, CODEX_POOL_RR_KEY=key)
    if is_kb:
        args = list(argv)
        # KB must bind remote items and launch every Codex process on this pool.
        env.update(KB_CODEX_CONFIG_OVERRIDES=json.dumps(overrides),
                   KB_POOL_ORIGIN=config["origin"].rstrip("/"),
                   KB_POOL_KEY_FILE=str(key_file),
                   KB_POOL_PRIVATE_HTTP="1" if private_http else "0")
    else:
        args = [argv[0], "exec", *[arg for override in overrides for arg in ("-c", override)], *argv[2:]]
    return args, env


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", default=str(ROOT / "bridge.json"), help="pool bridge config (default: repository bridge.json)")
    parser.add_argument("command", nargs=argparse.REMAINDER, help="codex exec ... or kb NAME [KB-options] codex ...")
    options = parser.parse_args()
    argv = options.command
    if argv[:1] == ["--"]:
        argv = argv[1:]
    try:
        args, env = command(options.config, argv)
        os.execvpe(args[0], args, env)
    except (OSError, ValueError, KeyError) as error:
        # Do not include file content or credentials in diagnostics.
        print(f"pool-rr: {type(error).__name__}: check command, config and client key", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
