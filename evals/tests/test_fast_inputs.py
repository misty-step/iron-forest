from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


class FocusedInputsTest(unittest.TestCase):
    def test_case_selection_does_not_touch_full_corpus(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "selected"
            manifest = json.loads((ROOT / "cases.json").read_text())
            selected = manifest["cases"][0]["id"]
            before = {path: path.read_bytes() for path in (ROOT / "tasks").rglob("*") if path.is_file()}
            subprocess.run([sys.executable, str(ROOT / "scripts/sync_tasks.py"), "--case", selected,
                            "--output-dir", str(output)], check=True)
            self.assertEqual({path.name for path in output.iterdir()}, {selected})
            self.assertEqual(json.loads((output / selected / "tests/scenario.json").read_text()), manifest["cases"][0])
            self.assertEqual({path: path.read_bytes() for path in (ROOT / "tasks").rglob("*") if path.is_file()}, before)
            unknown = subprocess.run([sys.executable, str(ROOT / "scripts/sync_tasks.py"), "--case", "not-a-case",
                                      "--output-dir", str(Path(temporary) / "unknown")], capture_output=True)
            self.assertNotEqual(unknown.returncode, 0)
            self.assertFalse((Path(temporary) / "unknown").exists())



if __name__ == "__main__":
    unittest.main()
