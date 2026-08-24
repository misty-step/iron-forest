from __future__ import annotations

import hashlib
import json
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "runtime"))

from runtime import grade as grade_module  # noqa: E402


def digest(path: Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(65536), b""):
            value.update(chunk)
    return value.hexdigest()


def write_bundle(root: Path, files: dict[str, str]) -> Path:
    bundle = root / "bundle"
    for relative, content in files.items():
        path = bundle / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
    manifest = {
        "schema": "forest.eval.artifact-bundle.v1",
        "files": {
            relative: {
                "sha256": digest(bundle / relative),
                "size": (bundle / relative).stat().st_size,
            }
            for relative in files
        },
    }
    (bundle / "manifest.json").write_text(json.dumps(manifest))
    return bundle


class VerifyBundleTest(unittest.TestCase):
    def test_valid_bundle_passes(self):
        with tempfile.TemporaryDirectory() as root:
            bundle = write_bundle(Path(root), {"state.json": "{}\n", "origin.git/config": "[core]\n"})
            self.assertIsNone(grade_module.verify_bundle(bundle))

    def test_missing_manifest_fails_closed(self):
        with tempfile.TemporaryDirectory() as root:
            bundle = Path(root) / "bundle"
            bundle.mkdir()
            (bundle / "state.json").write_text("{}\n")
            self.assertIn("missing manifest.json", grade_module.verify_bundle(bundle))

    def test_modified_file_fails_closed(self):
        with tempfile.TemporaryDirectory() as root:
            bundle = write_bundle(Path(root), {"state.json": "{}\n"})
            (bundle / "state.json").write_text("[]\n")
            self.assertIn("was modified", grade_module.verify_bundle(bundle))

    def test_extra_file_fails_closed(self):
        with tempfile.TemporaryDirectory() as root:
            bundle = write_bundle(Path(root), {"state.json": "{}\n"})
            (bundle / "unexpected.txt").write_text("extra\n")
            self.assertIn("do not match its manifest", grade_module.verify_bundle(bundle))

    def test_missing_declared_file_fails_closed(self):
        with tempfile.TemporaryDirectory() as root:
            bundle = write_bundle(Path(root), {"state.json": "{}\n", "runs/one.log": "x\n"})
            (bundle / "runs" / "one.log").unlink()
            self.assertIn("do not match its manifest", grade_module.verify_bundle(bundle))


if __name__ == "__main__":
    unittest.main()
