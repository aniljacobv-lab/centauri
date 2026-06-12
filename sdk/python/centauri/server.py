"""Find and launch a local Centauri server from Python.

This is the "zero terminal skills required" path:

    from centauri import Centauri
    db = Centauri.launch()        # finds centauri[.exe], starts it, connects

The launcher looks for the binary in this order: an explicit `binary=`
argument, the CENTAURI_BIN environment variable, the current directory,
the parents of this SDK (so a repo checkout Just Works), then PATH.
"""

import os
import shutil
import subprocess
import sys
import time
from pathlib import Path
from typing import List, Optional

from .errors import CentauriLaunchError

_BIN_NAMES = ["centauri.exe", "centauri"] if sys.platform == "win32" else ["centauri", "centauri.exe"]


def find_binary(binary: Optional[str] = None) -> str:
    """Locate the centauri server binary, or raise with instructions."""
    candidates: List[Path] = []
    if binary:
        candidates.append(Path(binary))
    env = os.environ.get("CENTAURI_BIN")
    if env:
        candidates.append(Path(env))
    for name in _BIN_NAMES:
        candidates.append(Path.cwd() / name)
    here = Path(__file__).resolve()
    for parent in list(here.parents)[:6]:  # sdk/python/centauri -> repo root
        for name in _BIN_NAMES:
            candidates.append(parent / name)
    for c in candidates:
        if c.is_file():
            return str(c)
    for name in _BIN_NAMES:
        hit = shutil.which(name)
        if hit:
            return hit
    raise CentauriLaunchError(
        "Couldn't find the Centauri server binary.\n"
        "  Fix any ONE of these and try again:\n"
        "  1. Build it:  go build -o centauri.exe ./cmd/centauri  (in the Centauri repo)\n"
        "  2. Tell me where it is:  Centauri.launch(binary=r'C:\\path\\to\\centauri.exe')\n"
        "  3. Or set the environment variable CENTAURI_BIN to its full path."
    )


def start_server(binary: Optional[str] = None, data: str = "centauri.log",
                 addr: str = ":7771", token: Optional[str] = None) -> subprocess.Popen:
    """Start `centauri serve` as a child process and return it."""
    path = find_binary(binary)
    cmd = [path, "serve", "-data", data, "-addr", addr]
    if token:
        cmd += ["-token", token]
    try:
        proc = subprocess.Popen(
            cmd,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.STDOUT,
            cwd=os.getcwd(),
        )
    except OSError as e:
        raise CentauriLaunchError(f"Found {path} but couldn't start it: {e}") from None
    return proc


def wait_until_up(ping, proc: subprocess.Popen, seconds: float = 10.0) -> None:
    """Poll ping() until the server answers or the wait runs out."""
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise CentauriLaunchError(
                f"The Centauri server exited immediately (code {proc.returncode}).\n"
                "  Most common cause: another server is already using that port.\n"
                "  Either connect to it instead — Centauri() — or launch on a free\n"
                "  port: Centauri.launch(addr=':7779')."
            )
        try:
            if ping():
                return
        except Exception:
            pass
        time.sleep(0.15)
    proc.terminate()
    raise CentauriLaunchError("The server started but never answered. "
                              "Check the data file path and try again.")
