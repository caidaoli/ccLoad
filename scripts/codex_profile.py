#!/usr/bin/env python3
"""Read a local native Codex installation without executing it or using the network.

Only the darwin-arm64 static layout verified in Codex 0.171.0 is recognized.
Changed or ambiguous layouts keep the existing built-in client version.
"""

import argparse
from pathlib import Path
import re
import shutil
import sys

from claude_profile import VERSION, UnknownProfile, atomic_update, native_image, version_tuple


BASELINE = "internal/codexauth/service.go"
CONSTANT = "DefaultClientVersion"
# The built-in originator is codex-tui; its anchor fails if a release renames it.
PATTERNS = {
    "MCP client": re.compile(rb"codex-mcp-client/(" + VERSION + rb")(?![0-9.])"),
    "doctor": re.compile(rb"codex-doctor/(" + VERSION + rb")(?![0-9.])"),
    "originator": re.compile(rb"codex-tui(" + VERSION + rb")(?![0-9.])"),
}


def extract_version(path):
    with native_image(path, "Codex") as data:
        found = set()
        for name, pattern in PATTERNS.items():
            matched = False
            for match in pattern.finditer(data):
                if data[match.end():match.end() + 1] in (b"-", b"+"):
                    raise UnknownProfile("unsupported Codex version suffix in " + name + " anchor")
                found.add(match[1])
                matched = True
            if not matched:
                raise UnknownProfile("missing recognized " + name + " anchor")
        if len(found) != 1:
            raise UnknownProfile("conflicting embedded Codex versions")
        return found.pop().decode("ascii")


def read_baseline(source):
    found = list(re.finditer(
        rb'(?m)^[ \t]*' + CONSTANT.encode("ascii") + rb'[ \t]*=[ \t]*"([^"\r\n]+)"', source
    ))
    if len(found) != 1:
        raise ValueError("expected exactly one Go constant " + CONSTANT)
    if not re.fullmatch(VERSION, found[0][1]):
        raise ValueError("invalid baseline version in " + CONSTANT)
    return found[0][1].decode("ascii"), found[0].span(1)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--check", action="store_true", help="report changes without writing (default)")
    modes.add_argument("--apply", action="store_true", help="atomically update a strictly newer same-major version")
    parser.add_argument("--repo-root", type=Path, default=Path(__file__).resolve().parent.parent)
    parser.add_argument("--codex", type=Path, help="local native executable (default: codex on PATH)")
    args = parser.parse_args(argv)
    try:
        baseline_path = args.repo_root / BASELINE
        original = baseline_path.read_bytes()
        baseline, (start, end) = read_baseline(original)
        discovered = args.codex if args.codex is not None else shutil.which("codex")
        if discovered is None:
            print("WARNING: local Codex executable not found; keeping baseline", file=sys.stderr)
            return 0
        try:
            binary = Path(discovered).resolve(strict=True)
            version = extract_version(binary)
        except (UnknownProfile, OSError, ValueError, RuntimeError) as error:
            print(f"WARNING: {error}; keeping baseline", file=sys.stderr)
            return 0
        print(f"Local Codex: {binary}")
        print(f"Baseline: {baseline}, extracted: {version}")
        current, candidate = version_tuple(baseline), version_tuple(version)
        if current[0] != candidate[0]:
            print("WARNING: unsupported Codex major version; keeping baseline", file=sys.stderr)
            return 0
        if candidate <= current:
            print("No update: local Codex is equal to or older than the baseline.")
            return 0
        print(f"{CONSTANT}: {baseline} -> {version}")
        if not args.apply:
            print("Check only: no files changed.")
            return 0
        atomic_update(baseline_path, original, original[:start] + version.encode("ascii") + original[end:])
        print(f"Updated {BASELINE} ({CONSTANT} only).")
        return 0
    except (OSError, ValueError) as error:
        print(f"ERROR: {error}; baseline not updated", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
