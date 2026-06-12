"""Centauri Python SDK — the friendly way to talk to a Centauri database.

Quickstart (three lines):

    from centauri import Centauri
    db = Centauri.launch()                       # or Centauri() if it's running
    db.add("toy:robot", {"price_cents": 500})

Read the README for the five-minute tour, or TUTORIAL.md for the
gentlest possible introduction.
"""

from .client import Centauri
from .errors import (CentauriAPIError, CentauriConnectionError, CentauriError,
                     CentauriLaunchError)
from .types import Context, Event

__version__ = "0.2.0"

__all__ = [
    "Centauri", "Event", "Context",
    "CentauriError", "CentauriConnectionError", "CentauriAPIError",
    "CentauriLaunchError",
    "__version__",
]
