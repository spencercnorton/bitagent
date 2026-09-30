"""Synthetic hashing limits use a fake monotonic clock, never wall-clock waits."""
import io
import json

import pytest

import media_publish as publisher
import torrent_metainfo as metainfo

MIB = 1024 * 1024
ANNOUNCE = "https://tracker.example.org/announce"


class Clock:
    def __init__(self):
        self.now = 0.0
        self.sleeps = []

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        assert 0 < seconds <= 1
        self.sleeps.append(seconds)
        self.now += seconds


def test_read_limit_is_one_mib_and_does_not_bank_idle_credit():
    clock = Clock()
    pacer = metainfo.HashPacer(1, clock=clock.monotonic, sleep=clock.sleep)
    source = io.BytesIO(b"a" * (4 * MIB))
    assert len(pacer.read(source, 4 * MIB)) == MIB
    assert clock.now == 0
    clock.now = 100
    assert len(pacer.read(source, MIB)) == MIB
    assert clock.now == 100
    assert len(pacer.read(source, MIB)) == MIB
    assert clock.now == 101
    assert clock.sleeps == [1]


def test_first_read_does_not_assume_the_monotonic_clocks_reference_point():
    clock = Clock()
    clock.now = -50
    pacer = metainfo.HashPacer(1, clock=clock.monotonic, sleep=clock.sleep)
    assert pacer.read(io.BytesIO(b"a"), MIB) == b"a"
    assert clock.now == -50
    assert clock.sleeps == []


def test_elapsed_read_time_counts_toward_the_budget():
    clock = Clock()
    starts = []

    class SlowReader(io.BytesIO):
        def read(self, size):
            starts.append(clock.now)
            chunk = super().read(size)
            clock.now += 0.4 if chunk else 0
            return chunk

    pacer = metainfo.HashPacer(1, clock=clock.monotonic, sleep=clock.sleep)
    stream = SlowReader(b"a" * (2 * MIB))
    assert len(pacer.read(stream, MIB)) == MIB
    assert len(pacer.read(stream, MIB)) == MIB
    assert pacer.read(stream, MIB) == b""
    assert starts == pytest.approx([0, 1, 2])
    assert sum(clock.sleeps) == pytest.approx(1.2)


def test_shared_pacer_covers_all_files_releases_and_independent_verification(tmp_path):
    root = tmp_path / "media"
    root.mkdir()
    contents = {"a.mkv": b"a" * (MIB + 123), "b.mkv": b"b" * (MIB // 2),
                "c.mkv": b"c" * (MIB + 81)}
    for name, data in contents.items():
        (root / name).write_bytes(data)
    inventory = {"version": 1, "releases": [
        {"source_id": "synthetic-1", "title": "Synthetic multipart movie", "kind": "movie", "files": ["a.mkv", "b.mkv"]},
        {"source_id": "synthetic-2", "title": "Synthetic movie", "kind": "movie", "files": ["c.mkv"]},
    ]}
    baseline = tmp_path / "unlimited"
    limited = tmp_path / "limited"
    publisher.publish_offline(inventory, root, ANNOUNCE, baseline)
    clock = Clock()
    pacer = metainfo.HashPacer(2, clock=clock.monotonic, sleep=clock.sleep)
    report = publisher.publish_offline(inventory, root, ANNOUNCE, limited, pacer=pacer)
    assert report["hashes_built"] == 2
    duration = sum(map(len, contents.values())) / MIB / 2
    assert clock.now == pytest.approx(duration)
    original = {p.name: p.read_bytes() for p in baseline.glob("*.torrent")}
    assert {p.name: p.read_bytes() for p in limited.glob("*.torrent")} == original
    assert publisher.verify_offline(inventory, root, limited, pacer=pacer) == 2
    assert clock.now == pytest.approx(2 * duration)


def test_resume_cache_hits_and_metadata_do_not_wait(tmp_path):
    source = tmp_path / "Synthetic.mkv"
    source.write_bytes(b"a" * (MIB + 9))
    inventory = {"version": 1, "releases": [{"source_id": "synthetic", "title": "Synthetic",
                 "kind": "movie", "files": [source.name]}]}
    output = tmp_path / "prepared"
    publisher.publish_offline(inventory, tmp_path, ANNOUNCE, output)
    clock = Clock()
    pacer = metainfo.HashPacer(1, clock=clock.monotonic, sleep=clock.sleep)

    def unexpected_read(*args):
        pytest.fail("cache reuse read media content")

    pacer.read = unexpected_read
    report = publisher.publish_offline(inventory, tmp_path, ANNOUNCE, output, resume=True, pacer=pacer)
    assert report["cache_reused"] == 1
    assert report["hashes_built"] == 0
    assert clock.now == 0
    assert clock.sleeps == []


@pytest.mark.parametrize("rate", ["nan", "inf", "-inf", "0", "-0", "-1", "invalid", "1e-320"])
def test_cli_rejects_invalid_hash_rates_before_reading_sources(tmp_path, monkeypatch, rate):
    monkeypatch.setattr(publisher, "_read_bounded", lambda *args: pytest.fail("invalid rate read input"))
    with pytest.raises(SystemExit) as exc:
        publisher.main(["--manifest", str(tmp_path / "absent.json"), "--root", str(tmp_path),
                        "--output", str(tmp_path / "prepared"), "--announce", ANNOUNCE,
                        "--hash-mib-per-second=" + rate])
    assert exc.value.code == 2
    assert not (tmp_path / "prepared").exists()


def test_unrepresentable_deadline_is_rejected_before_a_read():
    with pytest.raises(metainfo.TorrentError):
        metainfo.HashPacer(1e-320)


def test_default_hashing_does_not_sleep(tmp_path, monkeypatch):
    (tmp_path / "Synthetic.mkv").write_bytes(b"a" * (MIB + 13))
    monkeypatch.setattr(metainfo.time, "sleep", lambda *args: pytest.fail("unlimited hashing waited"))
    assert metainfo.build_private_torrent(tmp_path, ["Synthetic.mkv"], "Synthetic.mkv", ANNOUNCE).size == MIB + 13


def test_cli_threads_the_budget_into_preparation_and_verification(tmp_path, monkeypatch):
    (tmp_path / "Synthetic.mkv").write_bytes(b"a" * (MIB + 13))
    inventory = {"version": 1, "releases": [{"source_id": "synthetic", "title": "Synthetic",
                 "kind": "movie", "files": ["Synthetic.mkv"]}]}
    manifest = tmp_path / "inventory.json"
    manifest.write_text(json.dumps(inventory))
    clock = Clock()
    pacers = []

    def make_pacer(rate):
        assert rate == 2
        result = metainfo.HashPacer(rate, clock=clock.monotonic, sleep=clock.sleep)
        pacers.append(result)
        return result

    monkeypatch.setattr(publisher, "HashPacer", make_pacer)
    common = ["--root", str(tmp_path), "--output", str(tmp_path / "prepared"), "--hash-mib-per-second", "2"]
    assert publisher.main([*common, "--manifest", str(manifest), "--announce", ANNOUNCE]) == 0
    assert len(pacers) == 1
    duration = (MIB + 13) / MIB / 2
    assert clock.now == pytest.approx(duration)
    assert publisher.main([*common, "--verify"]) == 0
    assert len(pacers) == 2
    assert clock.now == pytest.approx(2 * duration)
