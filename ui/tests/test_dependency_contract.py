"""Keep the reviewed direct dependency pins and image lock in agreement."""

from __future__ import annotations

from importlib import metadata
from pathlib import Path
import re
import tomllib

import pytest


ROOT = Path(__file__).resolve().parent.parent
PIN_RE = re.compile(
    r"^(?P<name>[A-Za-z0-9_.-]+)(?:\[[^]]+\])?==(?P<version>[^\s\\]+)"
)


def _normalise_name(name: str) -> str:
    return re.sub(r"[-_.]+", "-", name).lower()


def _pins(path: Path) -> dict[str, str]:
    pins: dict[str, str] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        match = PIN_RE.match(line.strip())
        if match:
            pins[_normalise_name(match.group("name"))] = match.group("version")
    return pins


def _requirement_pins(requirements: list[str]) -> dict[str, str]:
    pins: dict[str, str] = {}
    for requirement in requirements:
        match = PIN_RE.fullmatch(requirement)
        assert match, f"test extra must be exactly pinned: {requirement}"
        pins[_normalise_name(match.group("name"))] = match.group("version")
    return pins


def _assert_every_pin_is_hashed(path: Path) -> None:
    lines = path.read_text(encoding="utf-8").splitlines()
    found = 0
    index = 0
    while index < len(lines):
        line = lines[index]
        if not PIN_RE.match(line.strip()):
            index += 1
            continue
        found += 1
        block = [line]
        while block[-1].rstrip().endswith("\\"):
            index += 1
            block.append(lines[index])
        assert "--hash=sha256:" in "\n".join(block), (
            f"unhashed requirement in {path.name}: {line.strip()}"
        )
        index += 1
    assert found


def test_direct_runtime_pins_match_hash_lock():
    direct = _pins(ROOT / "requirements.txt")
    locked = _pins(ROOT / "requirements.lock")

    assert direct
    assert direct.items() <= locked.items()


def test_direct_test_pins_match_hash_lock():
    direct = _pins(ROOT / "requirements-test.in")
    locked = _pins(ROOT / "requirements-test.lock")

    assert direct
    assert direct.items() <= locked.items()


def test_runtime_and_test_locks_do_not_resolve_shared_packages_differently():
    runtime = _pins(ROOT / "requirements.lock")
    test = _pins(ROOT / "requirements-test.lock")

    shared = runtime.keys() & test.keys()
    assert shared
    assert {name: runtime[name] for name in shared} == {
        name: test[name] for name in shared
    }


@pytest.mark.parametrize("lock_name", ["requirements.lock", "requirements-test.lock"])
def test_every_locked_distribution_has_a_sha256_hash(lock_name: str):
    _assert_every_pin_is_hashed(ROOT / lock_name)


def test_project_test_extra_matches_the_locked_test_inputs():
    project = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))
    extra = _requirement_pins(project["project"]["optional-dependencies"]["test"])
    direct = _pins(ROOT / "requirements-test.in")

    assert extra
    assert extra.items() <= direct.items()


@pytest.mark.parametrize(
    ("package", "expected"),
    [
        ("fastapi", "0.133.0"),
        ("starlette", "1.3.1"),
        ("jinja2", "3.1.6"),
    ],
)
def test_security_baseline_versions_are_installed(package: str, expected: str):
    assert metadata.version(package) == expected


def test_unused_multipart_parser_is_not_a_runtime_dependency():
    assert "python-multipart" not in _pins(ROOT / "requirements.txt")
    assert "python-multipart" not in _pins(ROOT / "requirements.lock")
