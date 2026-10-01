#!/usr/bin/env python3
"""Run codex exec through the pool's explicit round-robin endpoints."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import sys


ROOT = Path(__file__).resolve().parent.parent


def default_config():
    """The local pool's rr.json (or legacy kb-pool.json) when installed."""
    for name in ("rr.json", "kb-pool.json"):
        local = Path.home() / ".codex-pool" / name
        if local.is_file():
            return local
    raise ValueError("local pool config not found; run scripts/install-local.sh")


def command(config_path, argv):
    if not argv or Path(argv[0]).name != "codex" or argv[1:2] != ["exec"]:
        raise ValueError("usage: pool-rr [--config local pool config] codex exec ...")
    config_path = Path(config_path).expanduser().resolve()
    config = json.loads(config_path.read_text())
    spec = importlib.util.spec_from_file_location("compact_jsonl", ROOT / "scripts/compact-jsonl.py")
    compact = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(compact)
    origin = compact.config_origin(config)
    if not origin:
        raise ValueError("pool config needs base_url")
    base = compact.endpoint(origin).removesuffix("/responses")
    state = Path(config.get("state_dir", "state")).expanduser()
    if not state.is_absolute():
        state = config_path.parent / state
    key_file = Path(config.get("key_file", state / "client.key")).expanduser()
    if not key_file.is_absolute():
        key_file = config_path.parent / key_file
    key_file = key_file.resolve()
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
    standalone_search = config.get("rr_standalone_web_search", False)
    if type(standalone_search) is not bool:
        raise ValueError("rr_standalone_web_search must be a boolean")
    if standalone_search:
        # Enable only after deploying /_pool/rr/alpha/search on the server.
        # Lite-model Codex hides hosted web_search and needs this capability.
        provider["supports_standalone_web_search"] = True
    table = "{ " + ", ".join(k + " = " + json.dumps(v) for k, v in provider.items()) + " }"
    catalog = Path(config.get("rr_model_catalog", str(Path(os.environ.get("CODEX_HOME", Path.home() / ".codex")) / "models_cache.json"))).expanduser().resolve()
    if not catalog.is_file():
        raise ValueError("model catalog missing: run normal codex once or set rr_model_catalog in local pool config")
    overrides = ['model_provider="pool_rr"', "model_providers.pool_rr=" + table,
                 "model_catalog_json=" + json.dumps(str(catalog))]
    env = dict(os.environ, CODEX_POOL_RR_KEY=key)
    args = [argv[0], "exec", *[arg for override in overrides for arg in ("-c", override)], *argv[2:]]
    return args, env


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", default=None,
                        help="pool config (default: ~/.codex-pool/rr.json, or legacy kb-pool.json)")
    parser.add_argument("command", nargs=argparse.REMAINDER, help="codex exec ...")
    options = parser.parse_args()
    argv = options.command
    if argv[:1] == ["--"]:
        argv = argv[1:]
    try:
        args, env = command(options.config or default_config(), argv)
        os.execvpe(args[0], args, env)
    except (OSError, ValueError, KeyError) as error:
        # Do not include file content or credentials in diagnostics.
        print(f"pool-rr: {type(error).__name__}: check command, config and client key", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
