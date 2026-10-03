#!/usr/bin/env python3
"""Fail when hosted site code or deployment settings enter the public backend."""
from pathlib import Path
import re
import subprocess
import sys

SITE_PREFIXES = ("ui/", "internal/ui/", "deploy/", "site/")
SITE_DOCS = {
    "docs/ui-guide.md", "docs/system-tab.md", "docs/reference/dashboard-api.md",
    "docs/operations/invitations.md", "docs/operations/private-media.md",
    "docs/integrations/private-indexer.md", "docs/integrations/private-tracker-mode.md",
    "docs/integrations/definitions/bitagent-private.yml",
    "docs/integrations/definitions/bitagent-dht.yml",
}
SITE_CONFIG = re.compile(
    r"\b(?:UI_ENABLED|UI_DIR|UI_PYTHON|UI_LISTEN_ADDRESS|UI_RESTART_BACKOFF|"
    r"OPERATOR_HOSTS|PUBLIC_LIBRARY_HOSTS|DASHBOARD_API_KEY|TRUST_NPM_HEADERS|"
    r"REQUIRE_AUTH|INVITATIONS_ENABLED|PRIVATE_LIBRARY_ENABLED)\b"
)
SITE_LINK = re.compile(r"(?:\bui/|ui-guide\.md|system-tab\.md|dashboard-api\.md|"
                       r"operations/(?:invitations|private-media)\.md|"
                       r"integrations/(?:private-indexer|private-tracker-mode)\.md)")


def check(root, paths):
    failures = []
    for name in paths:
        path = root / name
        if not path.exists() and not path.is_symlink():
            continue  # Deletions in a working tree are not distribution inputs.
        if name.startswith(SITE_PREFIXES) or name in SITE_DOCS:
            failures.append((name, "hosted site belongs in its separate repository"))
            continue
        if name == "Dockerfile":
            data = path.read_text()
            if re.search(r"(?im)^FROM\s+python:|^COPY\s+(?:--[^ ]+\s+)?ui[/ ]|uvicorn", data):
                failures.append((name, "runtime includes the site or Python base"))
        current_doc = name in {"README.md", "CONTRIBUTING.md", "SECURITY.md"} or (
            name.startswith(("docs/", "examples/")) and path.suffix in {".md", ".yml", ".yaml"}
        )
        runtime_config = name == "examples/.env.example" or name.startswith(".github/workflows/")
        if (current_doc or runtime_config) and path.is_file():
            data = path.read_text()
            if SITE_CONFIG.search(data) or SITE_LINK.search(data):
                failures.append((name, "current backend instructions refer to site settings or files"))
    return failures


def main():
    root = Path.cwd()
    paths = subprocess.check_output(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"]
    ).decode().split("\0")
    failures = check(root, filter(None, paths))
    for name, reason in failures:
        print(f"{name}: {reason}")
    print(f"Headless boundary check: {len(failures)} finding(s)")
    return bool(failures)


if __name__ == "__main__":
    sys.exit(main())
