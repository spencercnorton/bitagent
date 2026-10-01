"""Exercise the distributable UI outside both its checkout and editable install."""
import ast
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[1]


def runtime_imports():
    modules = {path.stem: path for path in ROOT.glob("*.py")}
    pending = ["app", "media_publish"]
    found = set()
    while pending:
        name = pending.pop()
        if name in found:
            continue
        found.add(name)
        for node in ast.walk(ast.parse(modules[name].read_text())):
            if isinstance(node, ast.Import):
                imports = [alias.name.split(".")[0] for alias in node.names]
            elif isinstance(node, ast.ImportFrom) and node.module:
                imports = [node.module.split(".")[0]]
            else:
                continue
            pending.extend(imported for imported in imports if imported in modules)
    return sorted(found)


def run(args, *, cwd, env):
    result = subprocess.run(args, cwd=cwd, env=env, text=True, capture_output=True, timeout=90)
    assert result.returncode == 0, result.stdout + result.stderr
    return result


def test_sdist_wheel_imports_and_serves_assets_outside_checkout(tmp_path):
    project = tmp_path / "project"
    shutil.copytree(
        ROOT, project,
        ignore=shutil.ignore_patterns(".venv", "__pycache__", "build", "*.egg-info", "data"),
    )
    artifacts = tmp_path / "artifacts"
    artifacts.mkdir()
    env = {key: os.environ[key] for key in ("PATH", "SYSTEMROOT") if key in os.environ}
    env["PIP_CONFIG_FILE"] = os.devnull
    run(
        [sys.executable, "-I", "-c", "from setuptools.build_meta import build_sdist; "
         "import sys; build_sdist(sys.argv[1])", str(artifacts)],
        cwd=project, env=env,
    )
    sdists = list(artifacts.glob("*.tar.gz"))
    assert len(sdists) == 1
    run(
        [sys.executable, "-I", "-m", "pip", "--isolated", "wheel", "--no-index",
         "--no-deps", "--no-build-isolation", "--wheel-dir", str(artifacts), str(sdists[0])],
        cwd=tmp_path, env=env,
    )
    wheels = list(artifacts.glob("*.whl"))
    assert len(wheels) == 1
    installed = tmp_path / "installed"
    run(
        [sys.executable, "-I", "-m", "pip", "--isolated", "install", "--no-index",
         "--no-deps", "--target", str(installed), str(wheels[0])],
        cwd=tmp_path, env=env,
    )
    assets = {
        str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest()
        for name in ("static", "templates") for path in (ROOT / name).rglob("*")
        if path.is_file()
    }
    env.update({"DB_PATH": str(tmp_path / "isolated.db"), "REQUIRE_AUTH": "false",
                "OPERATOR_HOSTS": "localhost", "PUBLIC_LIBRARY_HOSTS": "library.localhost"})
    result = run(
        [sys.executable, "-I", "-c", INSTALLED_PROBE, str(installed),
         json.dumps(runtime_imports()), json.dumps(assets)],
        cwd=tmp_path, env=env,
    )
    proof = json.loads(result.stdout)
    assert proof["modules"] == len(runtime_imports())
    assert proof["assets"] == len(assets)
    assert proof["pages"] == 2


INSTALLED_PROBE = r'''
import asyncio, hashlib, importlib, json, pathlib, socket, sys
target = pathlib.Path(sys.argv[1]).resolve()
sys.path.insert(0, str(target))
def blocked(*args, **kwargs):
    raise AssertionError("Installed-wheel smoke test attempted network access")
socket.socket.connect = blocked
socket.socket.connect_ex = blocked
socket.create_connection = blocked
modules = json.loads(sys.argv[2])
for name in modules:
    module = importlib.import_module(name)
    assert pathlib.Path(module.__file__).resolve().parent == target, name
assets = json.loads(sys.argv[3])
for name, expected in assets.items():
    path = target / name
    assert path.is_file() and not path.is_symlink(), name
    assert hashlib.sha256(path.read_bytes()).hexdigest() == expected, name
import app, httpx
from importlib.metadata import version
assert app.__version__ == version("bitagent-ui")
async def smoke():
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app.app)) as client:
        for host, script in (("localhost", "app.js"), ("library.localhost", "library.js")):
            response = await client.get("http://" + host + "/")
            assert response.status_code == 200, (host, response.status_code)
            assert "text/html" in response.headers["content-type"]
            assert "/static/js/" + script in response.text
        for name in assets:
            if name.startswith("static/"):
                response = await client.get("http://localhost/" + name)
                assert response.status_code == 200, name
                assert hashlib.sha256(response.content).hexdigest() == assets[name], name
asyncio.run(smoke())
print(json.dumps({"modules": len(modules), "assets": len(assets), "pages": 2}))
'''
