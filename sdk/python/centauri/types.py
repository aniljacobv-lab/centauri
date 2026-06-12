"""Centauri SDK types.

Events come back as Event objects (attribute access, nice repr, real
datetimes) but never lose information: .raw always holds the exact dict
the server sent, so new server fields appear there before the SDK even
knows about them. That is the forward-compatibility rule of this SDK:
wrappers add convenience, .raw keeps truth.
"""

from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional, Union

# A "when" can be a datetime, an ISO string like "2026-03-15",
# or a raw UnixMicro integer. The SDK accepts all three everywhere.
When = Union[datetime, str, int, None]


def to_micros(when: When) -> Optional[int]:
    """Convert any accepted time shape to UnixMicro (None stays None)."""
    if when is None:
        return None
    if isinstance(when, int):
        return when
    if isinstance(when, datetime):
        if when.tzinfo is None:
            when = when.replace(tzinfo=timezone.utc)
        return int(when.timestamp() * 1_000_000)
    if isinstance(when, str):
        return when  # type: ignore[return-value]  # server parses RFC3339 / YYYY-MM-DD
    raise TypeError(f"Can't read {when!r} as a time. Use a datetime, "
                    f"'YYYY-MM-DD', an RFC3339 string, or a UnixMicro int.")


def from_micros(us: Optional[int]) -> Optional[datetime]:
    """UnixMicro -> aware UTC datetime (None/0 -> None)."""
    if not us:
        return None
    return datetime.fromtimestamp(us / 1_000_000, tz=timezone.utc)


@dataclass
class Event:
    """One immutable fact. Every fact knows its time, cause, and source."""

    event_id: str = ""
    subject: str = ""
    facet: str = "source"
    type: str = "OBSERVED"
    value: Dict[str, Any] = field(default_factory=dict)
    effective_time: int = 0
    effective_end: int = 0
    recorded_time: int = 0
    activation_time: int = 0
    superseded_by: str = ""
    provenance: str = "HUMAN_ENTRY"
    confidence: float = 1.0
    source_system: str = "python-sdk"
    source_ref: str = ""
    schema_id: str = ""
    raw: Dict[str, Any] = field(default_factory=dict, repr=False)

    # -- friendly views ------------------------------------------------
    @property
    def effective_at(self) -> Optional[datetime]:
        """When this fact became true in the world."""
        return from_micros(self.effective_time)

    @property
    def recorded_at(self) -> Optional[datetime]:
        """When Centauri learned about this fact."""
        return from_micros(self.recorded_time)

    @property
    def activated_at(self) -> Optional[datetime]:
        """When the facet acted on this fact (None = still pending)."""
        return from_micros(self.activation_time)

    @property
    def is_current(self) -> bool:
        """True if no newer fact has replaced this one."""
        return self.superseded_by == ""

    # -- conversion ----------------------------------------------------
    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "Event":
        e = cls(
            event_id=d.get("event_id", ""),
            subject=d.get("subject", ""),
            facet=d.get("facet", ""),
            type=d.get("type", ""),
            value=d.get("value") or {},
            effective_time=d.get("effective_time", 0) or 0,
            effective_end=d.get("effective_end", 0) or 0,
            recorded_time=d.get("recorded_time", 0) or 0,
            activation_time=d.get("activation_time", 0) or 0,
            superseded_by=d.get("superseded_by", "") or "",
            provenance=d.get("provenance", "") or "",
            confidence=d.get("confidence", 0.0) or 0.0,
            source_system=d.get("source_system", "") or "",
            source_ref=d.get("source_ref", "") or "",
            schema_id=d.get("schema_id", "") or "",
        )
        e.raw = d
        return e

    def to_payload(self) -> Dict[str, Any]:
        """The dict shape /v1/append expects."""
        out: Dict[str, Any] = {
            "subject": self.subject,
            "facet": self.facet,
            "type": self.type,
            "value": self.value,
            "provenance": self.provenance,
            "confidence": self.confidence,
            "source_system": self.source_system,
        }
        if self.event_id:
            out["event_id"] = self.event_id
        if self.effective_time:
            out["effective_time"] = self.effective_time
        if self.source_ref:
            out["source_ref"] = self.source_ref
        if self.schema_id:
            out["schema_id"] = self.schema_id
        return out


def events_from(payload: Any) -> List[Event]:
    """Turn a server list (possibly null) into Event objects."""
    if not payload:
        return []
    return [Event.from_dict(d) for d in payload]


class Context:
    """The full reasoning bundle for one subject (from /v1/context).

    Attributes mirror the server bundle; .raw holds everything verbatim.
    """

    def __init__(self, d: Dict[str, Any]):
        self.raw: Dict[str, Any] = d
        self.subject: str = d.get("subject", "")
        self.facts: List[Event] = events_from(d.get("facts"))
        self.history: List[Event] = events_from(d.get("history"))
        self.pending: List[Event] = events_from(d.get("pending"))
        self.disagreements: List[Dict[str, Any]] = d.get("disagreements") or []
        self.enrichments: Dict[str, Any] = d.get("enrichments") or {}
        self.causes: Dict[str, Any] = d.get("causes") or {}
        self.schemas: List[Dict[str, Any]] = d.get("schemas") or []
        conf = d.get("confidence") or {}
        self.confidence_min: float = conf.get("min", 0.0)
        self.confidence_mean: float = conf.get("mean", 0.0)

    def __repr__(self) -> str:
        return (f"<Context {self.subject!r}: {len(self.facts)} facts, "
                f"{len(self.pending)} pending, "
                f"{len(self.disagreements)} disagreements>")
