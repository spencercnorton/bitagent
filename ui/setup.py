"""Build the flat-layout application's assets beside its installed modules."""
from pathlib import Path
import stat

from setuptools import setup
from setuptools.command.build_py import build_py


class BuildPy(build_py):
    def run(self):
        super().run()
        source_root = Path(__file__).parent
        for name in ("static", "templates"):
            tree = source_root / name
            if tree.is_symlink() or not tree.is_dir():
                raise ValueError(f"Invalid application asset tree: {name}")
            for source in sorted(tree.rglob("*")):
                mode = source.lstat().st_mode
                if stat.S_ISDIR(mode):
                    continue
                if not stat.S_ISREG(mode):
                    raise ValueError(f"Application asset must be a regular file: {source.name}")
                destination = Path(self.build_lib) / source.relative_to(source_root)
                destination.parent.mkdir(parents=True, exist_ok=True)
                self.copy_file(str(source), str(destination))


setup(cmdclass={"build_py": BuildPy})
