#!/usr/bin/env python3
"""Exercise the public distribution guard against representative regressions."""
from pathlib import Path
import tempfile
import unittest

from check_headless_boundary import check


class HeadlessBoundaryTests(unittest.TestCase):
    def check_file(self, name, content=""):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content)
            return check(root, [name])

    def test_site_code_and_docs_cannot_return(self):
        for path in ("ui/app.py", "internal/ui/worker.go", "deploy/compose.yml",
                     "docs/operations/private-media.md", "docs/reference/dashboard-api.md"):
            with self.subTest(path=path):
                self.assertTrue(self.check_file(path))

    def test_site_build_and_settings_cannot_return(self):
        for path, content in (("Dockerfile", "FROM python:3.12-slim\n"),
                              ("examples/.env.example", "UI_ENABLED=true\n"),
                              ("README.md", "See [console](ui/README.md).\n")):
            with self.subTest(path=path):
                self.assertTrue(self.check_file(path, content))

    def test_core_apis_and_generic_processing_remain_allowed(self):
        self.assertFalse(self.check_file("internal/torznab/handler.go", "package torznab\n"))
        self.assertFalse(self.check_file("docs/configuration.md", "TORZNAB_API_KEY=example\n"))
        self.assertFalse(self.check_file("Dockerfile", "FROM alpine:3.23\nCOPY bitagent /usr/bin/\n"))

    def test_deleted_site_file_is_not_distributed(self):
        with tempfile.TemporaryDirectory() as temp:
            self.assertFalse(check(Path(temp), ["ui/app.py"]))


if __name__ == "__main__":
    unittest.main()
