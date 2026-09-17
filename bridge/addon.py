"""Route captured chatgpt.com requests; leave body and WebSocket bytes opaque."""

import ipaddress
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
        r.stream = True
        flow.metadata["pool_bridge_mode"] = self.mode
        # Log no query, body, headers, credentials, or resource IDs.
        logging.info("pool bridge: %s %s request", self.mode, r.method)
        if self.mode == "observe":
            return
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
        flow.response.stream = True
        if flow.metadata.get("pool_bridge_mode") == "pool":
            flow.response.headers.pop("set-cookie", None)
            flow.response.headers.pop("set-cookie2", None)

    def tcp_message(self, flow: tcp.TCPFlow):
        # websocket=false makes HTTP upgrades raw TCP. Discard only retained
        # history: mitmproxy sends the current TCPMessage object after this hook.
        # Keep that object and its bytes unchanged (no frame parsing/rebuilding).
        del flow.messages[:-1]


addons = [Bridge()]
