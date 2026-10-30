"""Behavioral tests for the local-only release profile updater CLI."""

import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest


HELPER = Path(__file__).with_name("claude_profile.py")
BASELINE = b'''package app
const (
    anthropicCLIVersion = "2.1.313"
    anthropicStainlessPackageVersion = "0.127.0"
    anthropicStainlessRuntimeVersion = "v26.11.0"
    platformOS = "Windows"
    platformArch = "x64"
)
// Preserve all unrelated bytes.\n'''


def native_fixture(cli="2.1.314", sdk="0.128.0", node="26.12.1"):
    return b"\xcf\xfa\xed\xfe" + b"\0" * 60 + (
        'PACKAGE_URL:"@anthropic-ai/claude-code",'
        'README_URL:"https://code.claude.com/docs/en/overview",'
        f'VERSION:"{cli}"\0'
        f'var sdkVersion="{sdk}";function headers(){{return {{'
        '"X-Stainless-Lang":"js","X-Stainless-Package-Version":sdkVersion,'
        '"X-Stainless-OS":getOS(),"X-Stainless-Arch":getArch(),'
        '"X-Stainless-Runtime":"node",'
        '"X-Stainless-Runtime-Version":globalThis.process.version??"unknown"}}\0'
        f'nodejsVersion\0{node}\0wordSize\0'
        f'bun/1.3.0 npm/? node/v{node} darwin arm64\0'
        f'headersUrl\0https://nodejs.org/download/release/v{node}/node-v{node}-headers.tar.gz\0'
    ).encode()


class ClaudeProfileCLITest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / "internal/app/anthropic_wire.go"
        self.source.parent.mkdir(parents=True)
        self.source.write_bytes(BASELINE)
        self.source.chmod(0o640)
        self.binary = self.root / "native-claude"
        self.binary.write_bytes(native_fixture())
        self.binary.chmod(0o755)

    def run_cli(self, *args, explicit=True, env=None):
        command = [sys.executable, str(HELPER), "--repo-root", str(self.root)]
        if explicit:
            command.extend(["--claude", str(self.binary)])
        return subprocess.run(command + list(args), capture_output=True, text=True, env=env, timeout=15)

    def assert_kept(self, result, code=0):
        self.assertEqual(result.returncode, code, result.stdout + result.stderr)
        self.assertEqual(self.source.read_bytes(), BASELINE)
        self.assertEqual(list(self.source.parent.glob(".claude-profile-*")), [])

    def test_default_and_check_are_read_only(self):
        for args in ((), ("--check",)):
            with self.subTest(args=args):
                before = self.source.stat().st_mtime_ns
                result = self.run_cli(*args)
                self.assert_kept(result)
                self.assertIn("2.1.313 -> 2.1.314", result.stdout)
                self.assertIn("Check only", result.stdout)
                self.assertEqual(self.source.stat().st_mtime_ns, before)

    def test_apply_updates_exact_complete_tuple_preserving_platform_and_permissions(self):
        result = self.run_cli("--apply")
        self.assertEqual(result.returncode, 0, result.stderr)
        expected = BASELINE.replace(b'"2.1.313"', b'"2.1.314"').replace(
            b'"0.127.0"', b'"0.128.0"'
        ).replace(b'"v26.11.0"', b'"v26.12.1"')
        self.assertEqual(self.source.read_bytes(), expected)
        self.assertEqual(stat.S_IMODE(self.source.stat().st_mode), 0o640)
        self.assertEqual(list(self.source.parent.glob(".claude-profile-*")), [])

    def test_equal_older_and_other_major_do_not_write(self):
        for version in ("2.1.313", "2.1.9", "1.99.999", "3.0.0"):
            with self.subTest(version=version):
                self.binary.write_bytes(native_fixture(cli=version))
                self.assert_kept(self.run_cli("--apply"))

    def test_semantic_version_order(self):
        self.binary.write_bytes(native_fixture(cli="2.1.1000"))
        result = self.run_cli("--apply")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(b'"2.1.1000"', self.source.read_bytes())

    def test_resolves_symlink_and_discovers_path_without_running_tools(self):
        marker = self.root / "executed"
        # Trap commands catch accidental attempts to execute, install or download.
        for name in ("curl", "wget", "npm", "node", "brew", "claude"):
            tool = self.root / name
            tool.write_text(f'#!/bin/sh\nprintf invoked > "{marker}"\nexit 99\n')
            tool.chmod(0o755)
        (self.root / "claude").unlink()
        (self.root / "claude").symlink_to(self.binary)
        result = self.run_cli("--apply", explicit=False, env={**os.environ, "PATH": str(self.root)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(str(self.binary), result.stdout)
        self.assertFalse(marker.exists())
        self.assertIn(b'"2.1.314"', self.source.read_bytes())

    def test_missing_autodiscovery_warns(self):
        result = self.run_cli("--apply", explicit=False, env={**os.environ, "PATH": str(self.root)})
        self.assert_kept(result)
        self.assertIn("WARNING", result.stderr)

    def test_unknown_incomplete_and_conflicting_profiles_warn_and_keep_baseline(self):
        complete = native_fixture()
        variants = {
            "unknown": complete[:64] + b"different layout",
            "incomplete": complete.split(b"headersUrl")[0],
            "CLI conflict": complete + native_fixture(cli="2.1.315"),
            "SDK conflict": complete + native_fixture(sdk="0.129.0"),
            "runtime mismatch": complete.replace(b"nodejsVersion\00026.12.1", b"nodejsVersion\00026.12.2"),
            "URL mismatch": complete.replace(b"node-v26.12.1-headers", b"node-v26.12.2-headers"),
            "runtime expression": complete.replace(b'globalThis.process.version??"unknown"', b'"v26.12.1"'),
            "SDK literal": complete.replace(b'Version":sdkVersion', b'Version":"0.128.0"'),
            "SDK reassign": complete.replace(b"function headers", b'sdkVersion="0.129.0";function headers'),
            "SDK ambiguous": complete.replace(b"function headers", b'var sdkVersion="0.128.0";function headers'),
            "SDK missing": complete.replace(b'var sdkVersion="0.128.0";', b""),
            "SDK NUL boundary": complete.replace(b"function headers", b"\0function headers"),
            "SDK wrong identifier": complete.replace(b"var sdkVersion=", b"var othersdkVersion="),
        }
        for name, data in variants.items():
            with self.subTest(name=name):
                self.binary.write_bytes(data)
                result = self.run_cli("--apply")
                self.assert_kept(result)
                self.assertIn("WARNING", result.stderr)

    def test_invalid_local_sources_warn_without_execution_or_changes(self):
        marker = self.root / "executed"
        self.binary.write_text(f'#!/bin/sh\nprintf invoked > "{marker}"\n')
        result = self.run_cli("--apply")
        self.assert_kept(result)
        self.assertIn("WARNING", result.stderr)
        self.assertIn("native executable", result.stderr)
        self.assertFalse(marker.exists())
        self.binary.unlink()
        self.assert_kept(self.run_cli("--apply"))
        self.binary.touch()
        self.binary.chmod(0o755)
        self.assert_kept(self.run_cli("--apply"))
        self.binary.unlink()
        self.binary.mkdir()
        self.assert_kept(self.run_cli("--apply"))

    def test_nonexecutable_local_source_warns(self):
        self.binary.chmod(0o644)
        self.assert_kept(self.run_cli("--apply"))

    def test_broken_and_looping_local_symlinks_warn(self):
        for target in (self.root / "missing-claude", self.binary):
            with self.subTest(target=target):
                self.binary.unlink()
                self.binary.symlink_to(target)
                result = self.run_cli("--apply")
                self.assert_kept(result)
                self.assertIn("WARNING", result.stderr)
                self.assertNotIn("Traceback", result.stderr)

    @unittest.skipUnless(hasattr(os, "mkfifo"), "requires POSIX special files")
    def test_special_file_warns_without_blocking(self):
        self.binary.unlink()
        os.mkfifo(self.binary, 0o755)
        self.assert_kept(self.run_cli("--apply"))

    @unittest.skipIf(hasattr(os, "geteuid") and os.geteuid() == 0, "root bypasses directory permissions")
    def test_unwritable_destination_keeps_complete_baseline(self):
        self.source.parent.chmod(0o500)
        try:
            self.assert_kept(self.run_cli("--apply"), code=1)
        finally:
            self.source.parent.chmod(0o700)

    def test_invalid_baseline_fails_before_any_update(self):
        for invalid in (
            BASELINE.replace(b"anthropicCLIVersion", b"missingVersion"),
            BASELINE.replace(b'"2.1.313"', b'"unknown"'),
            BASELINE + b'\nanthropicCLIVersion = "2.1.313"\n',
        ):
            with self.subTest(invalid=invalid):
                self.source.write_bytes(invalid)
                result = self.run_cli("--apply")
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertEqual(self.source.read_bytes(), invalid)
                self.assertEqual(list(self.source.parent.glob(".claude-profile-*")), [])


if __name__ == "__main__":
    unittest.main()
