"""Route captured chatgpt.com requests; leave body and WebSocket bytes opaque."""

import base64
import ipaddress
import json
import logging
from pathlib import Path
from urllib.parse import urlsplit

from mitmproxy import ctx, exceptions, http, tcp


def parse_origin(value: str, private_http: bool = False):
    origin = urlsplit(value)
    if (
        origin.scheme not in ("http", "https")
        or not origin.hostname
        or origin.username is not None
        or origin.password is not None
        or origin.path not in ("", "/")
        or origin.query
        or origin.fragment
    ):
        raise ValueError("pool origin must be an http(s) origin without credentials or path")
    port = origin.port or (443 if origin.scheme == "https" else 80)
    try:
        loopback = ipaddress.ip_address(origin.hostname).is_loopback
    except ValueError:
        loopback = origin.hostname == "localhost"
    if origin.scheme == "http" and not loopback and not private_http:
        raise ValueError("non-loopback HTTP requires a verified private tunnel and --private-http")
    return origin, port


# Codex >= 0.156 fetches this at startup and refuses to run unless the
# response lists its own ChatGPT account id (workspace routing discovery).
# The pool answers with the selected pooled account, so rewrite the id.
ACCOUNTS_CHECK_PATHS = ("/backend-api/wham/accounts/check", "/backend-api/api/codex/accounts/check")


def client_account_id(request: http.Request):
    """Return the caller's ChatGPT account id from its header or its own JWT."""
    for name in request.headers:
        if name.lower().replace("_", "-") == "chatgpt-account-id" and request.headers[name].strip():
            return request.headers[name].strip()
    token = request.headers.get("Authorization", "")
    parts = token.split(".")
    if not token.startswith("Bearer ") or len(parts) != 3:
        return ""
    try:
        payload = parts[1]
        claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
        return str(claims.get("https://api.openai.com/auth", {}).get("chatgpt_account_id", "") or "")
    except (ValueError, AttributeError):
        return ""


class Bridge:
    def load(self, loader):
        loader.add_option("pool_mode", str, "observe", "observe or pool")
        loader.add_option("pool_origin", str, "http://127.0.0.1:18473", "pool origin")
        loader.add_option("pool_key_file", str, "", "dedicated pool client key file")
        loader.add_option("pool_private_http", bool, False, "HTTP uses a verified private tunnel")

    def configure(self, updated):
        if not any(name.startswith("pool_") for name in updated):
            return
        if ctx.options.pool_mode not in ("observe", "pool"):
            raise exceptions.OptionsError("pool_mode must be observe or pool")
        self.mode = ctx.options.pool_mode
        self.key = ""
        if self.mode == "pool":
            try:
                self.origin, self.port = parse_origin(
                    ctx.options.pool_origin, ctx.options.pool_private_http
                )
                self.key = Path(ctx.options.pool_key_file).read_text().strip()
                if not self.key or any(c.isspace() for c in self.key):
                    raise ValueError("pool client key must be a nonempty single token")
            except (OSError, ValueError) as exc:
                raise exceptions.OptionsError(str(exc)) from exc

    def requestheaders(self, flow: http.HTTPFlow):
        r = flow.request
        # Local Capture keeps the original destination IP in request.host.
        # pretty_host reads HTTP Host / HTTP/2 :authority without altering it.
        if r.scheme != "https" or r.pretty_host.lower() != "chatgpt.com" or r.port != 443:
            return
        # Codex 0.156.1 validates its locally selected workspace before inference.
        # This identity-only discovery must use that login, not a pooled identity.
        if r.method == "GET" and r.path.partition("?")[0] == "/backend-api/wham/accounts/check":
            logging.info("pool bridge: direct workspace identity discovery")
            return
        r.stream = True
        flow.metadata["pool_bridge_mode"] = self.mode
        # Log no query, body, headers, credentials, or resource IDs.
        logging.info("pool bridge: %s %s request", self.mode, r.method)
        if self.mode == "observe":
            return
        if r.method == "GET" and r.path.split("?", 1)[0] in ACCOUNTS_CHECK_PATHS:
            account_id = client_account_id(r)
            if account_id:
                flow.metadata["pool_client_account_id"] = account_id
        for name in list(r.headers):
            n = name.lower().replace("_", "-")
            if n in {"authorization", "cookie", "cookie2", "actor", "openai-actor", "x-openai-actor", "chatgpt-account-id", "openai-organization", "openai-project", "x-api-key", "api-key"}:
                del r.headers[name]
        r.headers["Authorization"] = "Bearer " + self.key
        r.scheme = self.origin.scheme
        r.host = self.origin.hostname
        r.port = self.port
        r.host_header = self.origin.netloc

    def responseheaders(self, flow: http.HTTPFlow):
        rewrite = (flow.metadata.get("pool_bridge_mode") == "pool"
                   and flow.metadata.get("pool_client_account_id")
                   and flow.response.status_code == 200)
        flow.response.stream = not rewrite
        if flow.metadata.get("pool_bridge_mode") == "pool":
            flow.response.headers.pop("set-cookie", None)
            flow.response.headers.pop("set-cookie2", None)

    def response(self, flow: http.HTTPFlow):
        account_id = flow.metadata.get("pool_client_account_id")
        if flow.response.stream or not account_id or flow.metadata.get("pool_bridge_mode") != "pool":
            return
        try:
            body = json.loads(flow.response.get_text(strict=False) or "")
            accounts = body["accounts"]
        except (ValueError, KeyError, TypeError):
            logging.warning("pool bridge: accounts/check response not rewritten (unexpected body)")
            return
        matching = [a for a in accounts if isinstance(a, dict) and a.get("id") == account_id]
        if matching:
            body["accounts"] = matching[:1]
        elif accounts and isinstance(accounts[0], dict):
            entry = dict(accounts[0])
            user = str(entry.get("account_user_id") or "")
            if "__" in user:
                entry["account_user_id"] = user.rsplit("__", 1)[0] + "__" + account_id
            entry["id"] = account_id
            body["accounts"] = [entry]
        else:
            return
        flow.response.text = json.dumps(body)
        logging.info("pool bridge: accounts/check rewritten to client account")

    def tcp_message(self, flow: tcp.TCPFlow):
        # websocket=false makes HTTP upgrades raw TCP. Discard only retained
        # history: mitmproxy sends the current TCPMessage object after this hook.
        # Keep that object and its bytes unchanged (no frame parsing/rebuilding).
        del flow.messages[:-1]


addons = [Bridge()]
