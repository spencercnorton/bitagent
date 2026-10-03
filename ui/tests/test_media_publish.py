"""Real filesystem hashing and synthetic Plex inventory, with no network seeding."""
import base64
import hashlib
import json
import os
import stat
import sqlite3

import pytest

import media_publish as publisher
import torrent_metainfo as metainfo

ANNOUNCE = "https://tracker.example.org/announce"
PIECE = 65536


def write(root, path, data):
    file = root / path
    file.parent.mkdir(parents=True, exist_ok=True)
    file.write_bytes(data)
    return file


def inventory(files, kind="movie", source_id="synthetic-1"):
    return {"version": 1, "releases": [{"source_id": source_id, "title": "Synthetic media", "kind": kind, "files": files}]}


def test_single_file_real_piece_hashes_and_private_flag(tmp_path):
    content = bytes(range(256)) * 300
    write(tmp_path, "movie.mkv", content)
    built = metainfo.build_private_torrent(tmp_path, ["movie.mkv"], "movie.mkv", ANNOUNCE, PIECE)
    meta = metainfo.parse_metainfo(built.metainfo)
    assert set(meta) == {"announce", "info"}
    assert meta["announce"] == ANNOUNCE.encode()
    assert meta["info"]["private"] == 1
    assert meta["info"]["pieces"] == b"".join(hashlib.sha1(content[n:n + PIECE], usedforsecurity=False).digest() for n in range(0, len(content), PIECE))
    assert meta["info"]["length"] == len(content) == built.size
    assert built.info_hash == hashlib.sha1(metainfo.bencode(meta["info"]), usedforsecurity=False).hexdigest()
    meta["announce"] = b"https://tracker.example.org/announce?passkey=synthetic"
    assert metainfo.info_hash(metainfo.parse_metainfo(metainfo.encode_metainfo(meta))) == built.info_hash


def test_pack_hashes_cross_file_boundaries_and_matches_existing_tree(tmp_path):
    first = b"a" * (PIECE - 2)
    second = b"b" * 10
    write(tmp_path, "TV/Synthetic/Season 1/01.mkv", first)
    write(tmp_path, "TV/Synthetic/Season 1/02.mkv", second)
    files = ["TV/Synthetic/Season 1/02.mkv", "TV/Synthetic/Season 1/01.mkv"]
    catalog, seeding, artifacts = publisher.prepare_catalog(inventory(files, "season"), tmp_path, ANNOUNCE, PIECE)
    info = metainfo.parse_metainfo(next(iter(artifacts.values())))["info"]
    assert info["name"] == b"Season 1"
    assert info["files"] == [{"length": len(first), "path": [b"01.mkv"]}, {"length": len(second), "path": [b"02.mkv"]}]
    content = first + second
    assert info["pieces"] == b"".join(hashlib.sha1(content[n:n + PIECE], usedforsecurity=False).digest() for n in range(0, len(content), PIECE))
    mapping = seeding["releases"][0]
    assert mapping["save_path"] == "TV/Synthetic"
    assert not mapping["save_parent_of_root"]
    for item in mapping["files"]:
        assert (tmp_path / mapping["save_path"] / "/".join(item["torrent_path"])).read_bytes() == (tmp_path / item["source_path"]).read_bytes()
    assert catalog["releases"][0]["seed_verified"] is False
    assert str(tmp_path) not in json.dumps(catalog)


def test_output_owner_only_and_rehash_detects_changed_source(tmp_path):
    root = tmp_path / "media"
    root.mkdir()
    source = write(root, "Synthetic.mkv", b"synthetic original")
    output = tmp_path / "prepared"
    data = inventory(["Synthetic.mkv"])
    report = publisher.publish_offline(data, root, ANNOUNCE, output, PIECE)
    assert stat.S_IMODE(output.stat().st_mode) == 0o700
    assert all(stat.S_IMODE(file.stat().st_mode) == 0o600 for file in output.iterdir())
    assert publisher.verify_offline(data, root, output) == 1
    assert report["seed_verified"] is False
    source.write_bytes(b"synthetic modified")
    with pytest.raises(metainfo.TorrentError, match="no longer match"):
        publisher.verify_offline(data, root, output)
    with pytest.raises(FileExistsError):
        publisher.publish_offline(data, root, ANNOUNCE, output, PIECE)


@pytest.mark.parametrize("path", ["../secret", "/absolute", "a/../b", "a/./b", "a//b", "a\\b", "a/", "./b", "x\x00mkv"])
def test_unsafe_paths_rejected(tmp_path, path):
    with pytest.raises(metainfo.TorrentError):
        metainfo.build_private_torrent(tmp_path, [path], "Synthetic.mkv", ANNOUNCE, PIECE)


def test_file_and_parent_symlinks_and_fifo_rejected(tmp_path):
    outside = write(tmp_path, "outside.mkv", b"outside")
    (tmp_path / "link.mkv").symlink_to(outside)
    (tmp_path / "linked").symlink_to(tmp_path, target_is_directory=True)
    os.mkfifo(tmp_path / "pipe.mkv")
    for path in ("link.mkv", "linked/outside.mkv", "pipe.mkv"):
        with pytest.raises(metainfo.TorrentError):
            metainfo.build_private_torrent(tmp_path, [path], "Synthetic.mkv", ANNOUNCE, PIECE)


def test_mutation_of_earlier_part_during_release_hash_is_rejected(tmp_path, monkeypatch):
    first = write(tmp_path, "a.mkv", b"first")
    write(tmp_path, "b.mkv", b"second")
    original = metainfo._open_media
    calls = 0

    def mutate(root_fd, parts):
        nonlocal calls
        calls += 1
        if calls == 2:
            first.write_bytes(b"other")
        return original(root_fd, parts)

    monkeypatch.setattr(metainfo, "_open_media", mutate)
    with pytest.raises(metainfo.TorrentError, match="changed during release"):
        metainfo.build_private_torrent(tmp_path, ["a.mkv", "b.mkv"], "Synthetic", ANNOUNCE, PIECE)


@pytest.mark.parametrize("announce", ["http://tracker.example.org/announce", "https://public.example.org/a", "https://tracker.example.org/announce?token=x", "https://user:pass@tracker.example.org/announce", "https://tracker.example.org/announce#x", "https://tracker.example.org/announce\n"])
def test_announce_must_be_token_free_controlled_https_endpoint(tmp_path, announce):
    with pytest.raises(metainfo.TorrentError):
        metainfo.build_private_torrent(tmp_path, ["absent"], "Synthetic", announce, PIECE)


@pytest.mark.parametrize("encoded", [b"i03e", b"i-0e", b"i+2e", b"01:a", b"d1:bi1e1:ai2ee", b"d1:ai1e1:ai2ee", b"1:aX", b"l", b"2:a", b"l" * 17 + b"e" * 17])
def test_decoder_rejects_noncanonical_truncated_or_unbounded_data(encoded):
    with pytest.raises(metainfo.TorrentError):
        metainfo.bdecode(encoded)


def test_decoder_binary_keys_and_values_roundtrip():
    value = {b"announce": ANNOUNCE.encode(), b"info": {b"name": b"Synthetic.mkv", b"pieces": b"\x00\xff", b"private": 1}}
    encoded = metainfo.bencode(value)
    assert metainfo.bdecode(encoded) == value
    assert metainfo.encode_metainfo(metainfo.parse_metainfo(encoded)) == encoded


def plex_episode(number, path, media_id):
    return {"type": "episode", "ratingKey": str(number), "title": f"Episode {number}", "grandparentTitle": "Synthetic Show",
            "grandparentRatingKey": "500", "parentIndex": 1, "index": number,
            "Media": [{"id": media_id, "Part": [{"file": path}]}]}


def test_plex_inventory_real_parts_versions_ids_and_show_season_packs(tmp_path):
    write(tmp_path, "Movies/Synthetic.mkv", b"film")
    write(tmp_path, "TV/Synthetic Show/Season 1/01.mkv", b"episode one")
    write(tmp_path, "TV/Synthetic Show/Season 1/02.mkv", b"episode two")
    payloads = [{"type": "show", "ratingKey": "500", "Guid": [{"id": "tvdb://987"}, {"id": "tmdb://654"}]}, {"type": "movie", "ratingKey": "100", "title": "Synthetic film", "Guid": [{"id": "imdb://tt1234567"}, {"id": "tmdb://321"}],
                 "Media": [{"id": 1000, "Part": [{"file": "/container/movies/Synthetic.mkv"}]}]},
                plex_episode(1, "/container/tv/Synthetic Show/Season 1/01.mkv", 2001),
                plex_episode(2, "/container/tv/Synthetic Show/Season 1/02.mkv", 2002)]
    inv = publisher.inventory_from_plex(payloads, tmp_path, [("/container/movies", "Movies"), ("/container/tv", "TV")])
    assert [release["kind"] for release in inv["releases"]] == ["movie", "episode", "episode", "season", "show"]
    assert inv["releases"][0]["imdb_id"] == "tt1234567"
    assert inv["releases"][0]["tmdb_id"] == 321
    assert inv["releases"][3]["episodes"] == [1, 2]
    assert all(item["tvdb_id"] == 987 and item["tmdb_id"] == 654 for item in inv["releases"][1:])
    catalog, seeding, _ = publisher.prepare_catalog(inv, tmp_path, ANNOUNCE, PIECE)
    assert len(catalog["releases"]) == 5
    assert all(release["seed_verified"] is False for release in seeding["releases"])


def test_plex_missing_or_unmapped_parts_fail_instead_of_silent_omission(tmp_path):
    payload = plex_episode(1, "/unmapped/secret.mkv", 2001)
    with pytest.raises(metainfo.TorrentError, match="outside configured"):
        publisher.inventory_from_plex([payload], tmp_path, [("/media", "")])
    payload["Media"] = []
    with pytest.raises(metainfo.TorrentError, match="no downloadable"):
        publisher.inventory_from_plex([payload], tmp_path)


def test_plex_pagination_headers_token_and_no_download_mutations(tmp_path, monkeypatch):
    calls = []
    responses = [
        {"MediaContainer": {"Directory": [{"key": "1", "type": "movie"}]}},
        {"MediaContainer": {"totalSize": 2, "Metadata": [{"type": "movie", "ratingKey": "101", "title": "Synthetic one", "Media": [{"Part": [{"file": "/plex/one.mkv"}]}]}]}},
        {"MediaContainer": {"totalSize": 2, "Metadata": [{"type": "movie", "ratingKey": "102", "title": "Synthetic two", "Media": [{"Part": [{"file": "/plex/two.mkv"}]}]}]}},
    ]

    class Response:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            pass

        def read(self, limit):
            return json.dumps(responses.pop(0)).encode()

    class Opener:
        def open(self, request, timeout):
            calls.append(request)
            return Response()

    monkeypatch.setattr(publisher, "build_opener", lambda *handlers: Opener())
    inv = publisher.fetch_plex_inventory("https://plex.example.org", "synthetic-token", tmp_path, [("/plex", "")])
    assert len(inv["releases"]) == 2
    assert all(request.get_method() == "GET" for request in calls)
    assert all("synthetic-token" not in request.full_url for request in calls)
    assert calls[2].get_header("X-plex-container-start") == "1"
    assert calls[0].get_header("X-plex-token") == "synthetic-token"


def test_tampered_catalog_hash_and_artifact_are_rejected(tmp_path):
    root = tmp_path / "media"
    root.mkdir()
    write(root, "synthetic.mkv", b"synthetic")
    data = inventory(["synthetic.mkv"])
    output = tmp_path / "prepared"
    publisher.publish_offline(data, root, ANNOUNCE, output, PIECE)
    index = json.loads((output / "catalog-index.json").read_bytes())
    shard = output / index["shards"][0]["file"]
    catalog = json.loads(shard.read_bytes())
    catalog["releases"][0]["info_hash"] = "f" * 40
    shard.write_text(json.dumps(catalog))
    with pytest.raises(metainfo.TorrentError, match="(hash or size|missing from its checkpoint)"):
        publisher.verify_offline(data, root, output)
    catalog["releases"][0]["metainfo_base64"] = base64.b64encode(b"other").decode()
    shard.write_text(json.dumps(catalog))
    with pytest.raises(metainfo.TorrentError, match="differs from its artifact"):
        publisher.verify_offline(data, root, output)


def test_content_replacement_gets_distinct_immutable_source_id(tmp_path):
    source = write(tmp_path, "synthetic.mkv", b"original")
    data = inventory(["synthetic.mkv"])
    first, _, _ = publisher.prepare_catalog(data, tmp_path, ANNOUNCE, PIECE)
    source.write_bytes(b"replacement")
    second, seeding, _ = publisher.prepare_catalog(data, tmp_path, ANNOUNCE, PIECE)
    assert first["releases"][0]["source_id"] != second["releases"][0]["source_id"]
    assert seeding["releases"][0]["source_id"] == "synthetic-1"
    assert second["releases"][0]["source_id"] == "synthetic-1:" + second["releases"][0]["info_hash"]


def test_catalog_import_batches_are_bounded_and_keep_all_releases():
    items = [{"source_id": f"synthetic-{n}", "title": "Synthetic"} for n in range(1001)]
    batches = publisher.catalog_batches({"version": 1, "releases": items})
    assert [len(batch["releases"]) for batch in batches] == [1000, 1]
    assert [item for batch in batches for item in batch["releases"]] == items
    with pytest.raises(metainfo.TorrentError, match="too large"):
        publisher.catalog_batches({"releases": [{"metainfo_base64": "x" * publisher.MAX_IMPORT_BYTES}]})


def test_streaming_batches_cover_more_than_one_hundred_thousand_identities():
    produced = 0
    def items():
        nonlocal produced
        for number in range(120_001):
            produced += 1
            yield {"source_id": f"synthetic-{number}", "title": "Synthetic media", "kind": "episode"}
    iterator = publisher.iter_catalog_batches(items())
    first = next(iterator)
    assert len(first["releases"]) == 1000
    assert produced == 1001  # a single lookahead, not the full inventory
    count = len(first["releases"])
    shards = 1
    for batch in iterator:
        assert 1 <= len(batch["releases"]) <= 1000
        assert len(json.dumps(batch).encode()) <= publisher.MAX_IMPORT_BYTES
        count += len(batch["releases"])
        shards += 1
    assert count == 120_001 and shards == 121


def test_inventory_validation_supports_real_metadata_minimum_without_media_io():
    releases = [{"source_id": f"synthetic-{n}", "title": "Synthetic", "kind": "episode", "files": ["synthetic.mkv"]} for n in range(120_001)]
    assert len(publisher._inventory({"version": 1, "releases": releases})) == 120_001


def test_checkpoint_resume_reuses_only_unchanged_source_fingerprints(tmp_path, monkeypatch):
    root = tmp_path / "media"
    root.mkdir()
    source = write(root, "synthetic.mkv", b"original")
    output = tmp_path / "prepared"
    data = inventory(["synthetic.mkv"])
    first = publisher.publish_offline(data, root, ANNOUNCE, output, PIECE)
    assert first["hashes_built"] == 1
    assert not (output / "catalog.json").exists()
    original_builder = publisher.build_private_torrent
    calls = 0
    def builder(*args, **kwargs):
        nonlocal calls
        calls += 1
        return original_builder(*args, **kwargs)
    monkeypatch.setattr(publisher, "build_private_torrent", builder)
    second = publisher.publish_offline(data, root, ANNOUNCE, output, PIECE, resume=True)
    assert calls == 0 and second["cache_reused"] == 1
    old = source.stat()
    source.write_bytes(b"changed!")
    os.utime(source, ns=(old.st_atime_ns, old.st_mtime_ns))
    third = publisher.publish_offline(data, root, ANNOUNCE, output, PIECE, resume=True)
    assert calls == 1 and third["hashes_built"] == 1
    assert publisher.verify_offline(None, root, output) == 1


def test_interrupted_source_keeps_hash_checkpoint_without_publishing_complete_index(tmp_path, monkeypatch):
    root = tmp_path / "media"
    root.mkdir()
    write(root, "synthetic.mkv", b"synthetic")
    output = tmp_path / "prepared"
    release = inventory(["synthetic.mkv"])["releases"][0]
    def interrupted():
        yield release
        raise metainfo.TorrentError("synthetic source interruption")
    with pytest.raises(metainfo.TorrentError, match="source interruption"):
        publisher.publish_stream(interrupted(), root, ANNOUNCE, output, PIECE)
    assert not (output / "catalog-index.json").exists()
    assert (output / ".publisher.lock").exists()  # flock releases automatically, including process crashes
    monkeypatch.setattr(publisher, "build_private_torrent", lambda *a, **kw: pytest.fail("resume rehashed unchanged media"))
    report = publisher.publish_stream(iter([release]), root, ANNOUNCE, output, PIECE, resume=True)
    assert report["completeness"] == "complete" and report["cache_reused"] == 1


def test_missing_files_are_audited_and_never_claim_complete_inventory(tmp_path):
    root = tmp_path / "media"
    root.mkdir()
    output = tmp_path / "prepared"
    report = publisher.publish_offline(inventory(["absent.mkv"]), root, ANNOUNCE, output, PIECE)
    assert report["completeness"] == "incomplete"
    assert report["release_count"] == 0 and report["rejected"] == 1
    audit = [json.loads(line) for line in (output / report["audit"]).read_text().splitlines()]
    assert audit[-1]["source_id"] == "synthetic-1" and audit[-1]["status"] == "rejected"


def test_corrupt_checkpoint_artifact_is_rejected_instead_of_reused(tmp_path):
    root = tmp_path / "media"
    root.mkdir()
    write(root, "synthetic.mkv", b"synthetic")
    output = tmp_path / "prepared"
    data = inventory(["synthetic.mkv"])
    first = publisher.publish_offline(data, root, ANNOUNCE, output, PIECE)
    shard = json.loads((output / first["shards"][0]["file"]).read_bytes())
    (output / shard["releases"][0]["torrent_file"]).write_bytes(b"invalid")
    report = publisher.publish_offline(data, root, ANNOUNCE, output, PIECE, resume=True)
    assert report["cache_reused"] == 0 and report["rejected"] == 1


def test_optional_full_catalog_is_explicit(tmp_path):
    write(tmp_path, "synthetic.mkv", b"synthetic")
    output = tmp_path / "prepared"
    publisher.publish_offline(inventory(["synthetic.mkv"]), tmp_path, ANNOUNCE, output, PIECE, full_catalog=True)
    assert (output / "catalog.json").exists()


def test_streamed_plex_records_audit_every_empty_or_unmapped_asset(tmp_path, monkeypatch):
    write(tmp_path, "TV/Synthetic/Season 1/01.mkv", b"synthetic episode")
    responses = [
        {"MediaContainer": {"Directory": [{"key": "2", "type": "show"}]}},
        {"MediaContainer": {"totalSize": 2, "Metadata": [{"type": "show", "ratingKey": "500", "title": "Synthetic"}, {"type": "show", "ratingKey": "600", "title": "Empty show"}]}},
        {"MediaContainer": {"totalSize": 3, "Metadata": [{"type": "season", "ratingKey": "501", "parentRatingKey": "500", "index": 1}, {"type": "season", "ratingKey": "502", "parentRatingKey": "500", "index": 2}, {"type": "season", "ratingKey": "601", "parentRatingKey": "600", "index": 1}]}},
        {"MediaContainer": {"totalSize": 2, "Metadata": [plex_episode(1, "/plex/Synthetic/Season 1/01.mkv", 2001), plex_episode(2, "/unmapped/02.mkv", 2002)]}},
    ]
    class Response:
        def __enter__(self): return self
        def __exit__(self, *args): pass
        def read(self, limit): return json.dumps(responses.pop(0)).encode()
    class Opener:
        def open(self, request, timeout): return Response()
    monkeypatch.setattr(publisher, "build_opener", lambda *handlers: Opener())
    releases = publisher.iter_plex_releases("https://plex.example.org", "synthetic-token", tmp_path, [("/plex", "TV")])
    report = publisher.publish_stream(releases, tmp_path, ANNOUNCE, tmp_path / "prepared", PIECE)
    assert report["metadata_counts"] == {"show": 2, "season": 3, "episode": 2}
    assert report["emitted_counts"] == {"episode": 1}
    assert report["rejected_counts"] == {"episode": 1, "season": 3, "show": 2}
    assert report["completeness"] == "incomplete" and report["rejected"] == 6
    assert report["release_count"] + report["rejected"] == sum(report["metadata_counts"].values())


def test_pack_spanning_approved_subtrees_uses_actual_common_root(tmp_path):
    root = tmp_path / "library"
    root.mkdir()
    write(root, "drive-a/Synthetic/01.mkv", b"one")
    write(root, "drive-b/Synthetic/02.mkv", b"two")
    files = ["drive-a/Synthetic/01.mkv", "drive-b/Synthetic/02.mkv"]
    _, seeding, artifacts = publisher.prepare_catalog(inventory(files, "show"), root, ANNOUNCE, PIECE)
    meta = metainfo.parse_metainfo(next(iter(artifacts.values())))
    assert meta["info"]["name"] == b"library"
    assert seeding["releases"][0]["save_parent_of_root"] is True
    for mapping in seeding["releases"][0]["files"]:
        assert (root.parent / "/".join(mapping["torrent_path"])).read_bytes() == (root / mapping["source_path"]).read_bytes()


def test_live_output_lock_prevents_concurrent_publisher(tmp_path):
    import fcntl
    write(tmp_path, "synthetic.mkv", b"synthetic")
    output = tmp_path / "prepared"
    data = inventory(["synthetic.mkv"])
    publisher.publish_offline(data, tmp_path, ANNOUNCE, output, PIECE)
    with (output / ".publisher.lock").open("r+") as holder:
        fcntl.flock(holder.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        with pytest.raises(metainfo.TorrentError, match="another publisher"):
            publisher.publish_offline(data, tmp_path, ANNOUNCE, output, PIECE, resume=True)


def test_large_pack_piece_table_scales_without_reading_large_media():
    size = 600 * 1024**3
    length = metainfo.adaptive_piece_length(size)
    assert length == 8 * 1024**2
    assert 20 * ((size + length - 1) // length) <= metainfo.MAX_METAINFO_BYTES // 2
    with pytest.raises(metainfo.TorrentError, match="maximum piece length"):
        metainfo.adaptive_piece_length(10 * 1024**4)


def test_explicit_pack_exclusions_are_audited_as_declared_scope(tmp_path, monkeypatch):
    write(tmp_path, "synthetic.mkv", b"synthetic episode")
    responses = [
        {"MediaContainer": {"Directory": [{"key": "2", "type": "show"}]}},
        {"MediaContainer": {"totalSize": 1, "Metadata": [{"type": "show", "ratingKey": "500", "title": "Synthetic"}]}},
        {"MediaContainer": {"totalSize": 1, "Metadata": [{"type": "season", "ratingKey": "501", "parentRatingKey": "500", "index": 1}]}},
        {"MediaContainer": {"totalSize": 1, "Metadata": [plex_episode(1, "/plex/synthetic.mkv", 2001)]}},
    ]
    class Response:
        def __enter__(self): return self
        def __exit__(self, *args): pass
        def read(self, limit): return json.dumps(responses.pop(0)).encode()
    class Opener:
        def open(self, request, timeout): return Response()
    monkeypatch.setattr(publisher, "build_opener", lambda *handlers: Opener())
    records = publisher.iter_plex_releases("https://plex.example.org", "synthetic-token", tmp_path, [("/plex", "")], include_packs=False)
    report = publisher.publish_stream(records, tmp_path, ANNOUNCE, tmp_path / "prepared", PIECE)
    assert report["inventory_scope"]["include_packs"] is False
    assert report["excluded_counts"] == {"season": 1, "show": 1}
    assert report["rejected"] == 0 and report["release_count"] == 1
    assert report["completeness"] == "complete"  # only within the explicitly recorded scope
    audit = [json.loads(line) for line in (tmp_path / "prepared" / report["audit"]).read_text().splitlines()]
    assert sum(row.get("status") == "excluded" for row in audit) == 2


def test_interrupted_resume_preserves_prior_generation_for_actual_rehash_verification(tmp_path, monkeypatch):
    root = tmp_path / "media"
    root.mkdir()
    write(root, "one.mkv", b"one")
    write(root, "two.mkv", b"two")
    data = {"version": 1, "releases": [
        inventory(["one.mkv"], source_id="one")["releases"][0],
        inventory(["two.mkv"], source_id="two")["releases"][0],
    ]}
    output = tmp_path / "prepared"
    first = publisher.publish_offline(data, root, ANNOUNCE, output, PIECE)
    original_index = (output / "catalog-index.json").read_bytes()
    monkeypatch.setattr(publisher, "build_private_torrent", lambda *a, **kw: pytest.fail("unchanged resume rehashed media"))
    def interrupted():
        yield data["releases"][0]
        raise metainfo.TorrentError("synthetic interruption after cached source")
    with pytest.raises(metainfo.TorrentError, match="synthetic interruption"):
        publisher.publish_stream(interrupted(), root, ANNOUNCE, output, PIECE, resume=True)
    assert (output / "catalog-index.json").read_bytes() == original_index
    # This actually reads both old checkpoint rows and rehashes source files.
    assert publisher.verify_offline(None, root, output) == 2
    with sqlite3.connect(output / "publisher-checkpoint.sqlite3") as db:
        assert db.execute("SELECT count(*) FROM published_releases WHERE run_id=?", (first["run_id"],)).fetchone()[0] == 2
        assert db.execute("SELECT count(*) FROM published_releases").fetchone()[0] == 3
    second = publisher.publish_offline(data, root, ANNOUNCE, output, PIECE, resume=True)
    assert second["cache_reused"] == 2
    assert publisher.verify_offline(data, root, output) == 2


def test_legacy_checkpoint_schema_migrates_without_invalidating_completed_generation(tmp_path):
    write(tmp_path, "one.mkv", b"one")
    output = tmp_path / "prepared"
    data = inventory(["one.mkv"])
    first = publisher.publish_offline(data, tmp_path, ANNOUNCE, output, PIECE)
    with sqlite3.connect(output / "publisher-checkpoint.sqlite3") as db:
        db.execute("ALTER TABLE published_releases RENAME TO current_releases")
        db.execute("CREATE TABLE published_releases (source_id TEXT PRIMARY KEY, input_source_id TEXT NOT NULL, inventory TEXT NOT NULL, run_id TEXT NOT NULL)")
        db.execute("INSERT INTO published_releases SELECT * FROM current_releases")
        db.execute("DROP TABLE current_releases")
        db.execute("CREATE INDEX published_run ON published_releases(run_id,input_source_id)")
    def interrupted():
        yield data["releases"][0]
        raise metainfo.TorrentError("synthetic source interruption")
    with pytest.raises(metainfo.TorrentError, match="synthetic source interruption"):
        publisher.publish_stream(interrupted(), tmp_path, ANNOUNCE, output, PIECE, resume=True)
    assert publisher.verify_offline(None, tmp_path, output) == 1
    with sqlite3.connect(output / "publisher-checkpoint.sqlite3") as db:
        primary = tuple(row[1] for row in sorted(db.execute("PRAGMA table_info(published_releases)"), key=lambda row: row[5]) if row[5])
        assert primary == ("run_id", "source_id")
        assert db.execute("SELECT count(*) FROM published_releases WHERE run_id=?", (first["run_id"],)).fetchone()[0] == 1


@pytest.mark.parametrize("failure", [OSError, KeyboardInterrupt])
def test_interrupted_torrent_write_never_installs_partial_artifact_and_resume_recovers(tmp_path, monkeypatch, failure):
    write(tmp_path, "one.mkv", b"one")
    data = inventory(["one.mkv"])
    output = tmp_path / "prepared"
    writer = publisher._write_private
    def interrupted(path, content):
        if ".torrent." in path.name:
            path.write_bytes(content[:5])
            raise failure("synthetic write interruption")
        writer(path, content)
    monkeypatch.setattr(publisher, "_write_private", interrupted)
    if failure is KeyboardInterrupt:
        with pytest.raises(KeyboardInterrupt):
            publisher.publish_offline(data, tmp_path, ANNOUNCE, output, PIECE)
    else:
        assert publisher.publish_offline(data, tmp_path, ANNOUNCE, output, PIECE)["rejected"] == 1
    assert not list(output.glob("*.torrent"))
    assert not list(output.glob("*.torrent.*.tmp"))
    with sqlite3.connect(output / "publisher-checkpoint.sqlite3") as db:
        assert db.execute("SELECT count(*) FROM hash_cache").fetchone()[0] == 0
    monkeypatch.setattr(publisher, "_write_private", writer)
    report = publisher.publish_offline(data, tmp_path, ANNOUNCE, output, PIECE, resume=True)
    assert report["hashes_built"] == 1 and report["completeness"] == "complete"
    assert publisher.verify_offline(None, tmp_path, output) == 1


def test_verifier_rejects_duplicate_catalog_source_without_manifest(tmp_path):
    write(tmp_path, "one.mkv", b"one")
    write(tmp_path, "two.mkv", b"two")
    data = {"version": 1, "releases": [inventory(["one.mkv"], source_id="one")["releases"][0], inventory(["two.mkv"], source_id="two")["releases"][0]]}
    output = tmp_path / "prepared"
    report = publisher.publish_offline(data, tmp_path, ANNOUNCE, output, PIECE)
    shard = output / report["shards"][0]["file"]
    catalog = json.loads(shard.read_bytes())
    catalog["releases"][1] = catalog["releases"][0]
    shard.write_text(json.dumps(catalog))
    with pytest.raises(metainfo.TorrentError, match="duplicate source_id"):
        publisher.verify_offline(None, tmp_path, output)


@pytest.mark.parametrize("streamed", [False, True])
@pytest.mark.parametrize("page", [
    {"totalSize": 1, "Metadata": []},
    {"totalSize": "1", "Metadata": []},
    {"totalSize": True, "Metadata": []},
    {"totalSize": -1, "Metadata": []},
    {"totalSize": 0, "offset": 1, "Metadata": []},
    {"totalSize": 0, "offset": "0", "Metadata": []},
    {"totalSize": 0, "size": 1, "Metadata": []},
    {"totalSize": 0, "Metadata": [{"ratingKey": "1"}]},
])
def test_plex_pagination_refuses_partial_or_contradictory_inventory(tmp_path, monkeypatch, streamed, page):
    responses = [{"MediaContainer": {"Directory": [{"key": "1", "type": "movie"}]}}, {"MediaContainer": page}]
    class Response:
        def __enter__(self): return self
        def __exit__(self, *args): pass
        def read(self, limit): return json.dumps(responses.pop(0)).encode()
    class Opener:
        def open(self, request, timeout): return Response()
    monkeypatch.setattr(publisher, "build_opener", lambda *handlers: Opener())
    with pytest.raises(metainfo.TorrentError, match="Plex pagination"):
        if streamed:
            publisher.publish_stream(publisher.iter_plex_releases("https://plex.example.org", "synthetic-token", tmp_path), tmp_path, ANNOUNCE, tmp_path / "prepared", PIECE)
        else:
            publisher.fetch_plex_inventory("https://plex.example.org", "synthetic-token", tmp_path)
    if streamed:
        assert not (tmp_path / "prepared/catalog-index.json").exists()


@pytest.mark.parametrize("streamed", [False, True])
@pytest.mark.parametrize("second_total", [None, 2, 4])
def test_plex_total_change_mid_scan_is_an_interruption(tmp_path, monkeypatch, streamed, second_total):
    row = {"type": "movie", "ratingKey": "101", "title": "Synthetic", "Media": [{"Part": [{"file": "/plex/one.mkv"}]}]}
    write(tmp_path, "one.mkv", b"one")
    second = {"Metadata": [{**row, "ratingKey": "102"}]}
    if second_total is not None:
        second["totalSize"] = second_total
    responses = [{"MediaContainer": {"Directory": [{"key": "1", "type": "movie"}]}}, {"MediaContainer": {"totalSize": 3, "Metadata": [row]}}, {"MediaContainer": second}]
    class Response:
        def __enter__(self): return self
        def __exit__(self, *args): pass
        def read(self, limit): return json.dumps(responses.pop(0)).encode()
    class Opener:
        def open(self, request, timeout): return Response()
    monkeypatch.setattr(publisher, "build_opener", lambda *handlers: Opener())
    with pytest.raises(metainfo.TorrentError, match="total changed"):
        if streamed:
            records = publisher.iter_plex_releases("https://plex.example.org", "synthetic-token", tmp_path, [("/plex", "")])
            publisher.publish_stream(records, tmp_path, ANNOUNCE, tmp_path / "prepared", PIECE)
        else:
            publisher.fetch_plex_inventory("https://plex.example.org", "synthetic-token", tmp_path, [("/plex", "")])


def test_streamed_episodes_convert_only_their_own_series_metadata(tmp_path, monkeypatch):
    shows = [{"type": "show", "ratingKey": str(500 + n), "title": "Synthetic", "Guid": [{"id": f"tvdb://{1000 + n}"}]} for n in range(10)]
    seasons = [{"type": "season", "parentRatingKey": "500", "index": 1}]
    episodes = [plex_episode(n, f"/plex/{n}.mkv", 2000 + n) for n in range(1, 4)]
    responses = [{"MediaContainer": {"Directory": [{"key": "1", "type": "show"}]}}] + [{"MediaContainer": {"totalSize": len(rows), "Metadata": rows}} for rows in (shows, seasons, episodes)]
    class Response:
        def __enter__(self): return self
        def __exit__(self, *args): pass
        def read(self, limit): return json.dumps(responses.pop(0)).encode()
    class Opener:
        def open(self, request, timeout): return Response()
    monkeypatch.setattr(publisher, "build_opener", lambda *handlers: Opener())
    converter = publisher.inventory_from_plex
    observed = []
    def capture(payloads, *args):
        observed.append(payloads)
        return converter(payloads, *args)
    monkeypatch.setattr(publisher, "inventory_from_plex", capture)
    records = list(publisher.iter_plex_releases("https://plex.example.org", "synthetic-token", tmp_path, [("/plex", "")], include_packs=False))
    emitted = [record for record in records if record.get("kind") == "episode" and not record.get("_rejection")]
    assert len(emitted) == 3
    assert all(record["tvdb_id"] == 1000 for record in emitted)
    assert all(len(payloads) == 2 and payloads[0]["ratingKey"] == "500" for payloads in observed)


def plex_track(path, rating_key="900", media_id=901):
    return {"type": "track", "ratingKey": rating_key, "title": "Synthetic Track",
            "grandparentTitle": "Synthetic Artist", "parentTitle": "Synthetic Album",
            "Media": [{"id": media_id, "Part": [{"file": path}]}]}


def test_plex_music_versions_keep_explicit_parts_and_no_video_identifiers(tmp_path):
    track = plex_track("/plex/music/01.flac")
    track["Guid"] = [{"id": "imdb://tt1234567"}, {"id": "tvdb://123"}]
    track["Media"].append({"id": 902, "Part": [{"file": "/plex/music/01.mp3"}]})
    inventory = publisher.inventory_from_plex([track], tmp_path, [("/plex/music", "Music")])
    assert inventory == {"version": 1, "releases": [
        {"source_id": "plex:900:901", "kind": "music", "title": "Synthetic Artist - Synthetic Album - Synthetic Track", "files": ["Music/01.flac"]},
        {"source_id": "plex:900:902", "kind": "music", "title": "Synthetic Artist - Synthetic Album - Synthetic Track", "files": ["Music/01.mp3"]},
    ]}


@pytest.mark.parametrize("streamed", [False, True])
def test_plex_music_section_reads_tracks_only_and_preserves_page_headers(tmp_path, monkeypatch, streamed):
    write(tmp_path, "Music/01.flac", b"synthetic music")
    responses = [
        {"MediaContainer": {"Directory": [{"key": "9", "type": "artist"}]}},
        {"MediaContainer": {"totalSize": 1, "Metadata": [plex_track("/plex/music/01.flac")]}},
    ]
    requests = []
    class Response:
        def __enter__(self): return self
        def __exit__(self, *args): pass
        def read(self, limit): return json.dumps(responses.pop(0)).encode()
    class Opener:
        def open(self, request, timeout):
            requests.append(request)
            return Response()
    monkeypatch.setattr(publisher, "build_opener", lambda *handlers: Opener())
    if streamed:
        records = publisher.iter_plex_releases("https://plex.example.org", "synthetic-token", tmp_path,
                                              [("/plex/music", "Music")], section_ids={"9"}, include_music=True)
        report = publisher.publish_stream(records, tmp_path, ANNOUNCE, tmp_path / "prepared", PIECE)
        assert report["metadata_counts"] == report["emitted_counts"] == {"music": 1}
        assert report["completeness"] == "complete" and report["rejected"] == 0
        assert publisher.verify_offline(None, tmp_path, tmp_path / "prepared") == 1
    else:
        result = publisher.fetch_plex_inventory("https://plex.example.org", "synthetic-token", tmp_path, [("/plex/music", "Music")], include_music=True)
        assert [row["kind"] for row in result["releases"]] == ["music"]
    assert [request.full_url for request in requests] == [
        "https://plex.example.org/library/sections",
        "https://plex.example.org/library/sections/9/all?type=10&includeGuids=1",
    ]
    assert requests[-1].get_header("X-plex-container-start") == "0"
    assert requests[-1].get_header("X-plex-container-size") == "250"


@pytest.mark.parametrize("problem", ["unmapped", "empty", "wrong-kind", "missing-id"])
def test_streamed_music_rejections_remain_incomplete_and_auditable(tmp_path, monkeypatch, problem):
    track = plex_track("/plex/music/01.flac")
    if problem == "unmapped":
        track["Media"][0]["Part"][0]["file"] = "/outside/01.flac"
    elif problem == "empty":
        track["Media"] = []
    elif problem == "wrong-kind":
        track["type"] = "album"
    else:
        del track["ratingKey"]
    responses = [
        {"MediaContainer": {"Directory": [{"key": "9", "type": "artist"}]}},
        {"MediaContainer": {"totalSize": 1, "Metadata": [track]}},
    ]
    class Response:
        def __enter__(self): return self
        def __exit__(self, *args): pass
        def read(self, limit): return json.dumps(responses.pop(0)).encode()
    class Opener:
        def open(self, request, timeout): return Response()
    monkeypatch.setattr(publisher, "build_opener", lambda *handlers: Opener())
    records = publisher.iter_plex_releases("https://plex.example.org", "synthetic-token", tmp_path, [("/plex/music", "Music")], include_music=True)
    report = publisher.publish_stream(records, tmp_path, ANNOUNCE, tmp_path / "prepared", PIECE)
    assert report["completeness"] == "incomplete" and report["rejected"] == 1
    assert report["metadata_counts"] == report["rejected_counts"] == {"music": 1}
    assert report["release_count"] == 0


@pytest.mark.parametrize("kind", ["music", "generic"])
def test_explicit_nonvideo_leaf_checkpoint_resume_and_independent_rehash(tmp_path, kind):
    write(tmp_path, "unit/01.bin", b"synthetic first")
    source = write(tmp_path, "unit/02.bin", b"synthetic second")
    data = inventory(["unit/01.bin", "unit/02.bin"], kind)
    output = tmp_path / "prepared"
    first = publisher.publish_offline(data, tmp_path, ANNOUNCE, output, PIECE)
    assert first["emitted_counts"] == {kind: 1} and first["seed_verified"] is False
    second = publisher.publish_offline(data, tmp_path, ANNOUNCE, output, PIECE, resume=True)
    assert second["hashes_built"] == 0 and second["cache_reused"] == 1
    assert publisher.verify_offline(data, tmp_path, output) == 1
    source.write_bytes(b"modified second")
    with pytest.raises(metainfo.TorrentError, match="no longer match"):
        publisher.verify_offline(data, tmp_path, output)


def test_plex_music_is_explicit_opt_in_and_default_scope_is_unchanged(tmp_path, monkeypatch):
    class Response:
        def __enter__(self): return self
        def __exit__(self, *args): pass
        def read(self, limit):
            return json.dumps({"MediaContainer": {"Directory": [{"key": "9", "type": "artist"}]}}).encode()
    class Opener:
        def open(self, request, timeout):
            assert request.full_url == "https://plex.example.org/library/sections"
            return Response()
    monkeypatch.setattr(publisher, "build_opener", lambda *handlers: Opener())
    records = list(publisher.iter_plex_releases("https://plex.example.org", "synthetic-token", tmp_path))
    assert records == [{"_inventory_event": True, "counts": {}, "scope": {
        "source": "plex", "include_packs": True, "requested_sections": "all video sections"}}]
    with pytest.raises(metainfo.TorrentError, match="requested Plex media section"):
        list(publisher.iter_plex_releases("https://plex.example.org", "synthetic-token", tmp_path, section_ids={"9"}))


@pytest.mark.parametrize("field,value", [("title", []), ("title", "  "), ("parentTitle", {}), ("grandparentTitle", 123)])
def test_plex_track_title_context_rejects_nontext_and_empty_title(tmp_path, field, value):
    track = plex_track("/plex/music/01.flac")
    track[field] = value
    with pytest.raises(metainfo.TorrentError, match="title context"):
        publisher.inventory_from_plex([track], tmp_path, [("/plex/music", "Music")])
