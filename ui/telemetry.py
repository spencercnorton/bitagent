"""Typed, source-aware telemetry primitives for the operator dashboard.

The dashboard must distinguish a measured zero from an unavailable source.
Every surfaced fact therefore carries the same envelope and every upstream
probe is run through one bounded concurrency/timeout policy.
"""
from __future__ import annotations

import asyncio
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
import logging
from typing import Any, Awaitable, Callable, Generic, Literal, Mapping, TypeVar


logger = logging.getLogger("bitagent-ui.telemetry")

T = TypeVar("T")
MetricStatus = Literal["ok", "partial", "unavailable", "stale", "error"]
Probe = tuple[str, Callable[[], Awaitable[Any]]]


def snapshot_timestamp() -> str:
    """Return an RFC 3339 UTC timestamp shared by one telemetry snapshot."""
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


@dataclass(frozen=True, slots=True)
class MetricEnvelope(Generic[T]):
    """JSON-safe contract for one measured or derived operator fact."""

    value: T | None
    status: MetricStatus
    source: str
    observed_at: str
    stale: bool = False
    error: str | None = None

    def as_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(frozen=True, slots=True)
class ProbeResult(Generic[T]):
    """Result of a single bounded source probe.

    Probe failures deliberately contain only stable, non-sensitive messages;
    unexpected exception details go to server logs, never the API response.
    """

    value: T | None
    status: MetricStatus
    source: str
    observed_at: str
    error: str | None = None

    @property
    def ok(self) -> bool:
        return self.status == "ok"

    def envelope(
        self,
        value: Any = None,
        *,
        source: str | None = None,
        status: MetricStatus | None = None,
        error: str | None = None,
    ) -> MetricEnvelope[Any]:
        if not self.ok:
            return MetricEnvelope(
                value=None,
                status=self.status,
                source=source or self.source,
                observed_at=self.observed_at,
                error=self.error,
            )
        return MetricEnvelope(
            value=value,
            status=status or "ok",
            source=source or self.source,
            observed_at=self.observed_at,
            error=error,
        )


class TelemetryUnavailable(RuntimeError):
    """A source completed, but could not provide a trustworthy value."""


async def collect_bounded_probes(
    probes: Mapping[str, Probe],
    *,
    observed_at: str,
    timeout_seconds: float,
    max_concurrency: int,
) -> dict[str, ProbeResult[Any]]:
    """Run a fixed probe set concurrently with per-probe timeouts.

    ``max_concurrency`` is enforced even if future snapshots add more sources.
    A timeout or one broken probe cannot block or cancel the remaining sources.
    """
    if timeout_seconds <= 0:
        raise ValueError("timeout_seconds must be positive")
    if max_concurrency <= 0:
        raise ValueError("max_concurrency must be positive")

    semaphore = asyncio.Semaphore(max_concurrency)

    async def run(name: str, source: str, call: Callable[[], Awaitable[Any]]):
        try:
            async with semaphore:
                value = await asyncio.wait_for(call(), timeout=timeout_seconds)
            return name, ProbeResult(
                value=value,
                status="ok",
                source=source,
                observed_at=observed_at,
            )
        except TimeoutError:
            return name, ProbeResult(
                value=None,
                status="error",
                source=source,
                observed_at=observed_at,
                error=f"probe timed out after {timeout_seconds:g}s",
            )
        except TelemetryUnavailable as exc:
            return name, ProbeResult(
                value=None,
                status="unavailable",
                source=source,
                observed_at=observed_at,
                error=str(exc) or "source unavailable",
            )
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.exception("telemetry probe %s failed", name)
            return name, ProbeResult(
                value=None,
                status="error",
                source=source,
                observed_at=observed_at,
                error="probe failed",
            )

    completed = await asyncio.gather(
        *(run(name, source, call) for name, (source, call) in probes.items())
    )
    return dict(completed)


def unavailable_metric(
    *,
    source: str,
    observed_at: str,
    error: str,
) -> MetricEnvelope[Any]:
    return MetricEnvelope(
        value=None,
        status="unavailable",
        source=source,
        observed_at=observed_at,
        error=error,
    )
