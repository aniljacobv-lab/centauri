"""HTTP plumbing for the Centauri SDK.

Pure standard library (urllib), so `pip install` never fights you over
dependencies. Everything user-facing lives in client.py; this module
only knows how to move JSON and SSE bytes with friendly errors.

This is also the SDK's extension point: a new server endpoint needs no
rewrite anywhere — client code (yours included) can always call
`transport.get("/v1/new-thing", param=1)` the day the server ships it.
"""

import json
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, Iterator, Optional

from .errors import CentauriAPIError, CentauriConnectionError


class Transport:
    def __init__(self, base_url: str, token: Optional[str] = None,
                 timeout: float = 10.0, retries: int = 2):
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout
        self.retries = retries

    # -- internals -------------------------------------------------------
    def _request(self, method: str, path: str, body: Optional[bytes],
                 timeout: Optional[float] = None) -> urllib.request.Request:
        req = urllib.request.Request(self.base_url + path, data=body, method=method)
        req.add_header("Accept", "application/json")
        if body is not None:
            req.add_header("Content-Type", "application/json")
        if self.token:
            req.add_header("Authorization", "Bearer " + self.token)
        return req

    def _send(self, method: str, path: str, body: Optional[bytes]) -> Any:
        last_err: Optional[Exception] = None
        attempts = self.retries + 1 if method == "GET" else 1  # never retry writes
        for attempt in range(attempts):
            try:
                req = self._request(method, path, body)
                with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                    data = resp.read()
                    return json.loads(data) if data else None
            except urllib.error.HTTPError as e:
                # The server answered with an error — don't retry, explain.
                try:
                    msg = json.loads(e.read()).get("error", e.reason)
                except Exception:
                    msg = str(e.reason)
                raise CentauriAPIError(e.code, msg, path) from None
            except (urllib.error.URLError, OSError, TimeoutError) as e:
                last_err = e
                if attempt < attempts - 1:
                    time.sleep(0.2 * (attempt + 1))
        raise CentauriConnectionError(self.base_url, last_err)

    @staticmethod
    def _qs(params: Dict[str, Any]) -> str:
        clean = {k: v for k, v in params.items() if v is not None and v != ""}
        return "?" + urllib.parse.urlencode(clean) if clean else ""

    # -- public surface ----------------------------------------------------
    def get(self, path: str, **params: Any) -> Any:
        """GET a JSON endpoint: transport.get('/v1/stats')"""
        return self._send("GET", path + self._qs(params), None)

    def post(self, path: str, body: Any) -> Any:
        """POST a JSON body: transport.post('/v1/append', {...})"""
        return self._send("POST", path, json.dumps(body).encode("utf-8"))

    def stream(self, path: str, **params: Any) -> Iterator[Dict[str, Any]]:
        """Yield JSON objects from a Server-Sent Events endpoint, forever
        (until the server closes or the caller stops iterating)."""
        url = path + self._qs(params)
        try:
            req = self._request("GET", url, None)
            resp = urllib.request.urlopen(req, timeout=None)  # streams have no deadline
        except urllib.error.HTTPError as e:
            try:
                msg = json.loads(e.read()).get("error", e.reason)
            except Exception:
                msg = str(e.reason)
            raise CentauriAPIError(e.code, msg, path) from None
        except (urllib.error.URLError, OSError) as e:
            raise CentauriConnectionError(self.base_url, e) from None
        try:
            for raw in resp:
                line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
                if line.startswith("data: "):
                    try:
                        yield json.loads(line[len("data: "):])
                    except json.JSONDecodeError:
                        continue  # tolerate partial frames; the log is truth
        finally:
            resp.close()
