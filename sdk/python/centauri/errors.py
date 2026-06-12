"""Centauri SDK errors.

Every error message tries to tell you what went wrong AND what to do
about it. If an error ever leaves you stuck, that's a bug in the SDK —
please report the message you saw.
"""

from typing import Optional


class CentauriError(Exception):
    """Base class for everything the Centauri SDK raises on purpose."""


class CentauriConnectionError(CentauriError):
    """Could not reach the Centauri server at all."""

    def __init__(self, url: str, cause: Optional[Exception] = None):
        self.url = url
        self.cause = cause
        super().__init__(
            f"Couldn't reach Centauri at {url}.\n"
            f"  Is the server running? Three ways to fix this:\n"
            f"  1. Start it from a terminal:  centauri serve -data centauri.log\n"
            f"  2. Or let Python start it:    db = Centauri.launch()\n"
            f"  3. Or check the address:      Centauri(url='http://host:port')\n"
            + (f"  (underlying error: {cause})" if cause else "")
        )


class CentauriAPIError(CentauriError):
    """The server answered, but said no. Check .status and .message."""

    def __init__(self, status: int, message: str, path: str = ""):
        self.status = status
        self.message = message
        self.path = path
        hint = ""
        if status == 401:
            hint = "\n  Hint: this server wants a token — Centauri(token='...')."
        elif status == 403:
            hint = "\n  Hint: this is a read-only follower; write to the primary instead."
        elif status == 404:
            hint = "\n  Hint: that endpoint doesn't exist — is the server an older Centauri version?"
        elif status == 422:
            hint = "\n  Hint: the data was rejected — the message above says exactly why."
        super().__init__(f"Centauri said no ({status}) on {path}: {message}{hint}")


class CentauriLaunchError(CentauriError):
    """Centauri.launch() couldn't find or start the server binary."""
