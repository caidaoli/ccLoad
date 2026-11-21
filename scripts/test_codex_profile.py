"""Behavioral tests for the local-only Codex client version updater CLI."""

import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


HELPER = Path(__file__).with_name("codex_profile.py")
BASELINE = b'''package codexauth
const (
    DefaultClientVersion    = "0.170.0"
    DefaultOriginator       = "codex-tui"
    DefaultUserAgent        = DefaultOriginator + "/" + DefaultClientVersion + " (Mac OS 27.2.1; arm64)"
)
// Preserve all unrelated bytes.\n'''


def native_fixture(version="0.171.0"):
    return b"\xcf\xfa\xed\xfe" + b"\0" * 60 + (
        f"\0codex-mcp-client/{version}\0\0"
        f"codex-doctor/{version}version response exceeds size limit"
        f"clicodex-tui{version}failed to start embedded app server"
    ).encode()


class CodexProfileCLITest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / "internal/codexauth/service.go"
        self.source.parent.mkdir(parents=True)
        self.source.write_bytes(BASELINE)
        self.binary = self.root / "native-codex"
        self.binary.write_bytes(native_fixture())
        self.binary.chmod(0o755)

    def run_cli(self, *args, explicit=True, env=None):
        command = [sys.executable, str(HELPER), "--repo-root", str(self.root)]
        if explicit:
            command.extend(["--codex", str(self.binary)])
        return subprocess.run(command + list(args), capture_output=True, text=True, env=env, timeout=15)

    def assert_kept(self, result, code=0):
        self.assertEqual(result.returncode, code, result.stdout + result.stderr)
        self.assertEqual(self.source.read_bytes(), BASELINE)

    def test_check_is_read_only(self):
        result = self.run_cli("--check")
        self.assert_kept(result)
        self.assertIn("0.170.0 -> 0.171.0", result.stdout)

    def test_apply_updates_only_client_version(self):
        result = self.run_cli("--apply")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.source.read_bytes(), BASELINE.replace(b'"0.170.0"', b'"0.171.0"'))

    def test_equal_older_and_other_major_do_not_write(self):
        for version in ("0.170.0", "0.99.9", "1.0.0"):
            with self.subTest(version=version):
                self.binary.write_bytes(native_fixture(version))
                self.assert_kept(self.run_cli("--apply"))

    def test_discovers_path_symlink_without_running_tools(self):
        marker = self.root / "executed"
        for name in ("curl", "npm", "brew"):
            tool = self.root / name
            tool.write_text(f'#!/bin/sh\nprintf invoked > "{marker}"\nexit 99\n')
            tool.chmod(0o755)
        (self.root / "codex").symlink_to(self.binary)
        result = self.run_cli("--apply", explicit=False, env={**os.environ, "PATH": str(self.root)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(marker.exists())
        self.assertIn(b'"0.171.0"', self.source.read_bytes())

    def test_unknown_conflicting_and_script_sources_keep_baseline(self):
        complete = native_fixture()
        variants = {
            "missing anchor": complete.replace(b"codex-doctor/", b"codex-other/"),
            "renamed originator": complete.replace(b"clicodex-tui", b"clicodex-cli"),
            "conflict": complete + native_fixture("0.172.0"),
            "trailing digit": complete.replace(b"codex-tui0.171.0f", b"codex-tui0.171.01"),
            "script": b"#!/bin/sh\nexec node codex.js\n",
        }
        for name, data in variants.items():
            with self.subTest(name=name):
                self.binary.write_bytes(data)
                result = self.run_cli("--apply")
                self.assert_kept(result)
                self.assertIn("WARNING", result.stderr)

    def test_invalid_baseline_fails(self):
        invalid = BASELINE.replace(b'"0.170.0"', b'"unknown"')
        self.source.write_bytes(invalid)
        result = self.run_cli("--apply")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(self.source.read_bytes(), invalid)


if __name__ == "__main__":
    unittest.main()
