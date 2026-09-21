#!/usr/bin/env python3
"""Send prepared JSONL input items to the pool's RR compaction route.

Each nonempty line is one Responses API input item, already prepared by the
caller. This is not a Codex rollout reader: no history replay or normalization.
"""

import argparse
import ipaddress
import json
import os
from pathlib import Path
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request


def request_body(source, model, instructions):
    items = []
    for number, line in enumerate(source, 1):
        line = line.strip()
        if not line:
            continue
        try:
            json.loads(line)
        except (ValueError, UnicodeDecodeError):
            raise ValueError(f"invalid JSON on line {number}") from None
        # Keep the original JSON bytes, including unknown fields and numbers.
        items.append(line)
    items.append(b'{"type":"compaction_trigger"}')
    envelope = json.dumps({"model": model, "instructions": instructions,
                           "stream": True, "store": False,
                           "reasoning": {"effort": "low"}}, ensure_ascii=False).encode()
    return envelope[:-1] + b',"input":[' + b','.join(items) + b']}'


def events(source):
    data = []
    for raw in source:
        line = raw.decode("utf-8").rstrip("\r\n")
        if not line:
            if data:
                text = "\n".join(data)
                data = []
                if text != "[DONE]":
                    yield json.loads(text)
        elif line.startswith("data:"):
            value = line[5:]
            data.append(value[1:] if value.startswith(" ") else value)


def compaction_item(source):
    items = []
    for event in events(source):
        kind = event.get("type")
        if kind in ("error", "response.failed", "response.incomplete"):
            raise ValueError(f"upstream returned {kind}")
        if kind == "response.output_item.done":
            item = event.get("item", {})
            if item.get("type") in ("compaction", "compaction_summary", "context_compaction"):
                items.append(item)
        if kind == "response.completed":
            if len(items) != 1 or not isinstance(items[0].get("encrypted_content"), str) or not items[0]["encrypted_content"]:
                raise ValueError("upstream completed without one encrypted compaction item")
            return items[0]
    raise ValueError("stream ended before response.completed")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def endpoint(origin, private_http):
    url = urllib.parse.urlsplit(origin)
    if (url.scheme not in ("https", "http") or not url.hostname or url.username is not None
            or url.password is not None or url.path not in ("", "/") or url.query or url.fragment):
        raise ValueError("--origin must be an http(s) origin without credentials or path")
    try:
        local = ipaddress.ip_address(url.hostname).is_loopback
    except ValueError:
        local = url.hostname == "localhost"
    if url.scheme == "http" and not local and not private_http:
        raise ValueError("remote HTTP requires --private-http for a verified private tunnel")
    return origin.rstrip("/") + "/_pool/rr/responses"


def connection(args):
    # Explicit legacy origins remain standalone; otherwise share bridge.json.
    config = {}
    config_path = None
    if args.pool_config or not args.origin:
        config_path = Path(args.pool_config or Path(__file__).resolve().parent.parent / "bridge.json").expanduser().resolve()
        config = json.loads(config_path.read_text())
    origin = args.origin or config.get("origin")
    if not origin:
        raise ValueError("set origin in --pool-config or pass --origin")
    url = endpoint(origin, args.private_http or config.get("private_http", False))
    if args.key_file:
        key_file = Path(args.key_file).expanduser()
    else:
        key_file = Path(config.get("key_file") or Path(config.get("state_dir", "state")) / "client.key").expanduser()
        if config_path and not key_file.is_absolute():
            key_file = config_path.parent / key_file
    return url, key_file


def save(item, destination):
    data = json.dumps(item, ensure_ascii=False, indent=2) + "\n"
    if destination == "-":
        sys.stdout.write(data)
        return
    path = Path(destination)
    descriptor, temporary = tempfile.mkstemp(prefix=".compact-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as out:
            out.write(data)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("jsonl", help="prepared input JSONL file, or - for stdin")
    parser.add_argument("--model", required=True, help="upstream model, sent unchanged")
    parser.add_argument("--pool-config", help="shared pool config (default: repository bridge.json when --origin is omitted)")
    parser.add_argument("--origin", help="override pool origin, without /v1")
    parser.add_argument("--private-http", action="store_true")
    parser.add_argument("--key-file", help="override pool client key file")
    parser.add_argument("--output", default="-", help="compaction item JSON file; default stdout")
    parser.add_argument("--instructions", default="Preserve the information in the provided conversation.")
    parser.add_argument("--timeout", type=float, default=180)
    args = parser.parse_args()
    try:
        if args.jsonl != "-" and args.output != "-" and Path(args.jsonl).resolve() == Path(args.output).resolve():
            raise ValueError("output must differ from input JSONL")
        url, key_file = connection(args)
        if args.jsonl == "-":
            body = request_body(sys.stdin.buffer, args.model, args.instructions)
        else:
            with open(args.jsonl, "rb") as source:
                body = request_body(source, args.model, args.instructions)
        key = key_file.read_text().strip()
        if not key or any(c.isspace() for c in key):
            raise ValueError("client key must be a nonempty single token")
        request = urllib.request.Request(url, data=body, headers={
            "Authorization": "Bearer " + key, "Content-Type": "application/json",
            "Originator": "codex_cli_rs",
        })
        # Exactly one generation request: do not replay, retry or follow redirects.
        with urllib.request.build_opener(NoRedirect()).open(request, timeout=args.timeout) as response:
            item = compaction_item(response)
        save(item, args.output)
        return 0
    except urllib.error.HTTPError as exc:
        print(f"compact-jsonl: upstream HTTP {exc.code}", file=sys.stderr)
    except (urllib.error.URLError, TimeoutError):
        print("compact-jsonl: connection failed or timed out", file=sys.stderr)
    except UnicodeError:
        print("compact-jsonl: invalid UTF-8", file=sys.stderr)
    except json.JSONDecodeError:
        print("compact-jsonl: invalid upstream event JSON", file=sys.stderr)
    except (ValueError, OSError) as exc:
        print(f"compact-jsonl: {exc}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
