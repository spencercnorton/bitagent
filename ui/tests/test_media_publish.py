"""Real filesystem hashing and synthetic Plex inventory, with no network seeding."""
import base64
import hashlib
import json
import os
import stat

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
    catalog = publisher.publish_offline(data, root, ANNOUNCE, output, PIECE)
    assert stat.S_IMODE(output.stat().st_mode) == 0o700
    assert all(stat.S_IMODE(file.stat().st_mode) == 0o600 for file in output.iterdir())
    assert publisher.verify_offline(data, root, output) == 1
    assert catalog["releases"][0]["seed_verified"] is False
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
    catalog = json.loads((output / "catalog.json").read_bytes())
    catalog["releases"][0]["info_hash"] = "f" * 40
    (output / "catalog.json").write_text(json.dumps(catalog))
    with pytest.raises(metainfo.TorrentError, match="hash or size"):
        publisher.verify_offline(data, root, output)
    catalog["releases"][0]["metainfo_base64"] = base64.b64encode(b"other").decode()
    (output / "catalog.json").write_text(json.dumps(catalog))
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
