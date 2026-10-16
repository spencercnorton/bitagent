#!/usr/bin/env python3
"""Fail if embedded model/capture/queue runtimes re-enter the main dependency graph."""
import pathlib
import subprocess

root = pathlib.Path(__file__).resolve().parents[1]
deps = subprocess.check_output(["go", "list", "-deps", "."], cwd=root, text=True).splitlines()
blocked = ("/llmprovider", "/llmmatch", "/llmstage", "/llmreadiness", "/llmeval", "/llmcapture", "/llmwork", "/onnx", "/tensorflow", "/torch")
found = [d for d in deps if any(part in d for part in blocked)]
if found:
    raise SystemExit("Embedded runtime dependency: " + ", ".join(found))
print("Core dependency graph has no embedded inference, capture, or legacy job runtime.")
