"""Single source of truth for the UI version (one tag series with the core).

`app.py` imports `__version__` for the FastAPI `version=` field, and
`pyproject.toml` reads it via `[tool.setuptools.dynamic]` so the packaged
metadata, the running app, and the git tag never drift. Bump this on release
and tag the merge commit `vX.Y.Z` to match.
"""

__version__ = "2.10.1"
