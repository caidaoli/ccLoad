import importlib.util
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("update_homebrew", Path(__file__).with_name("update-homebrew.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class UpdateHomebrewTest(unittest.TestCase):
    def setUp(self):
        self.formula = (ROOT / "Formula/ccload.rb").read_text()
        self.assets = [f"ccload-{os}-{arch}" for os in ("darwin", "linux") for arch in ("arm64", "amd64")]
        self.checksums = "\n".join(f"{n:064x}  {name}" for n, name in enumerate(self.assets, 1))

    def test_updates_all_platform_checksums_and_version(self):
        result = module.update_formula(self.formula, "v99.0.0", self.checksums)
        self.assertIn('version "99.0.0"', result)
        for n, name in enumerate(self.assets, 1):
            self.assertRegex(result, name + r'"\n\s+sha256 "' + f"{n:064x}" + '"')
        self.assertEqual(result, module.update_formula(result, "v99.0.0", self.checksums))

    def test_rejects_non_stable_tags_and_downgrades(self):
        for tag in ("v99.0.0-beta.1", "99.0.0", "v1.0.0", 'v99.0.0"'):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                module.update_formula(self.formula, tag, self.checksums)

    def test_rejects_missing_duplicate_and_invalid_checksums(self):
        for checksums in ("", self.checksums.splitlines()[0], self.checksums + "\n" + self.checksums,
                          self.checksums.replace("0", "z", 1)):
            with self.subTest(checksums=checksums), self.assertRaises(ValueError):
                module.update_formula(self.formula, "v99.0.0", checksums)

    def test_rejects_incomplete_formula(self):
        with self.assertRaises(ValueError):
            module.update_formula(self.formula.replace("ccload-linux-arm64", "unknown"), "v99.0.0", self.checksums)


if __name__ == "__main__":
    unittest.main()
