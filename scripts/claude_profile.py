#!/usr/bin/env python3
"""Read a local native Claude installation without executing it or using the network.

Only the darwin-arm64 static layout verified in Claude Code 2.1.312/2.1.314 is recognized.
Changed or ambiguous layouts keep the existing complete software baseline.
"""

import argparse
import mmap
import os
from pathlib import Path
import re
import shutil
import stat
import sys
import tempfile


VERSION = rb"[0-9]+\.[0-9]+\.[0-9]+"
IDENTIFIER = rb"[A-Za-z_$][A-Za-z0-9_$]*"
CONSTANTS = (
    "anthropicCLIVersion",
    "anthropicStainlessPackageVersion",
    "anthropicStainlessRuntimeVersion",
)
PATTERNS = {
    "CLI": re.compile(
        rb'PACKAGE_URL:"@anthropic-ai/claude-code",'
        rb'README_URL:"https://code\.claude\.com/docs/en/overview",VERSION:"('
        + VERSION + rb')"'
    ),
    "SDK": re.compile(rb'"X-Stainless-Package-Version":([^,}\x00]{1,128})[,}]'),
    "node header": re.compile(
        rb'"X-Stainless-Lang":"js","X-Stainless-Package-Version":('
        + IDENTIFIER + rb'),"X-Stainless-OS":[^{}\x00]{1,512}'
        rb'"X-Stainless-Runtime":"node","X-Stainless-Runtime-Version":([^}\x00]{1,128})}'
    ),
    "node report": re.compile(rb"nodejsVersion\x00(" + VERSION + rb")\x00wordSize\x00"),
    "node agent": re.compile(
        rb"bun/" + VERSION + rb" npm/\? node/v(" + VERSION + rb") darwin arm64\x00"
    ),
    "node headers URL": re.compile(
        rb"headersUrl\x00https://nodejs\.org/download/release/v("
        + VERSION + rb")/node-v(" + VERSION + rb")-headers\.tar\.gz\x00"
    ),
}


class UnknownProfile(Exception):
    """A valid local executable has an unrecognized or inconsistent layout."""


def sdk_binding(data, position, identifier):
    if not re.fullmatch(IDENTIFIER, identifier):
        raise UnknownProfile("unsupported SDK package version expression")
    prefix = data[max(0, position - 4096):position].rsplit(b"\0", 1)[-1]
    name = re.escape(identifier)
    declarations = re.findall(
        rb'(?:^|[^A-Za-z0-9_$])var ' + name + rb'="(' + VERSION + rb')";', prefix
    )
    assignments = re.findall(rb"(?:^|[^A-Za-z0-9_$])" + name + rb"=", prefix)
    if len(declarations) != 1 or len(assignments) != 1:
        raise UnknownProfile("missing or ambiguous SDK package version binding")
    return declarations[0]


def native_header(data):
    if data[:4] in (
        b"\x7fELF", b"\xfe\xed\xfa\xce", b"\xce\xfa\xed\xfe",
        b"\xfe\xed\xfa\xcf", b"\xcf\xfa\xed\xfe",
        b"\xca\xfe\xba\xbe", b"\xbe\xba\xfe\xca",
        b"\xca\xfe\xba\xbf", b"\xbf\xba\xfe\xca",
    ):
        return True
    if data[:2] == b"MZ" and len(data) >= 64:
        offset = int.from_bytes(data[60:64], "little")
        return data[offset:offset + 4] == b"PE\0\0"
    return False


def extract_profile(path):
    if not stat.S_ISREG(path.stat().st_mode) or not os.access(path, os.X_OK):
        raise ValueError("Claude source must be a regular executable file")
    with path.open("rb") as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or not os.access(path, os.X_OK):
            raise ValueError("Claude source must be a regular executable file")
        if info.st_size < 4 or info.st_size > 512 * 1024 * 1024:
            raise ValueError("Claude executable size is outside the supported range")
        with mmap.mmap(source.fileno(), 0, access=mmap.ACCESS_READ) as data:
            if not native_header(data):
                raise ValueError("Claude source is not a native executable; scripts are unsupported")
            seen = {}
            for name, pattern in PATTERNS.items():
                for match in pattern.finditer(data):
                    value = match[1]
                    if name in ("SDK", "node header"):
                        if name == "node header" and match[2] != b'globalThis.process.version??"unknown"':
                            raise UnknownProfile("unsupported SDK node runtime expression")
                        value = sdk_binding(data, match.start(), value)
                    elif name == "node headers URL" and value != match[2]:
                        raise UnknownProfile("inconsistent node headers URL versions")
                    if name in seen and seen[name] != value:
                        raise UnknownProfile("conflicting " + name + " versions")
                    seen[name] = value
                if name not in seen:
                    raise UnknownProfile("missing recognized " + name + " anchor")
            if seen["SDK"] != seen["node header"]:
                raise UnknownProfile("SDK node header differs from package version bindings")
            if len({seen["node report"], seen["node agent"], seen["node headers URL"]}) != 1:
                raise UnknownProfile("inconsistent embedded node compatibility versions")
            return tuple(value.decode("ascii") for value in (
                seen["CLI"], seen["SDK"], b"v" + seen["node report"]
            ))


def read_baseline(source):
    profile = []
    matches = []
    for name in CONSTANTS:
        found = list(re.finditer(
            rb'(?m)^[ \t]*' + name.encode("ascii") + rb'[ \t]*=[ \t]*"([^"\r\n]+)"', source
        ))
        if len(found) != 1:
            raise ValueError("expected exactly one Go constant " + name)
        value = found[0][1]
        pattern = rb"v" + VERSION if name == CONSTANTS[2] else VERSION
        if not re.fullmatch(pattern, value):
            raise ValueError("invalid baseline version in " + name)
        profile.append(value.decode("ascii"))
        matches.append(found[0].span(1))
    return tuple(profile), matches


def version_tuple(value):
    return tuple(int(part) for part in value.split("."))


def atomic_update(path, original, updated):
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=path.parent, prefix=".claude-profile-", delete=False) as output:
            temporary = Path(output.name)
            os.fchmod(output.fileno(), stat.S_IMODE(path.stat().st_mode))
            output.write(updated)
            output.flush()
            os.fsync(output.fileno())
        if path.read_bytes() != original:
            raise ValueError("baseline changed during extraction; refusing to overwrite it")
        os.replace(temporary, path)
        temporary = None
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--check", action="store_true", help="report changes without writing (default)")
    modes.add_argument("--apply", action="store_true", help="atomically update a strictly newer same-major profile")
    parser.add_argument("--repo-root", type=Path, default=Path(__file__).resolve().parent.parent)
    parser.add_argument("--claude", type=Path, help="local native executable (default: claude on PATH)")
    args = parser.parse_args(argv)
    try:
        baseline_path = args.repo_root / "internal/app/anthropic_wire.go"
        original = baseline_path.read_bytes()
        baseline, spans = read_baseline(original)
        discovered = args.claude if args.claude is not None else shutil.which("claude")
        if discovered is None:
            print("WARNING: local Claude executable not found; keeping baseline", file=sys.stderr)
            return 0
        try:
            binary = Path(discovered).resolve(strict=True)
            profile = extract_profile(binary)
        except (UnknownProfile, OSError, ValueError, RuntimeError) as error:
            print(f"WARNING: {error}; keeping baseline", file=sys.stderr)
            return 0
        print(f"Local Claude: {binary}")
        print(f"Baseline: CLI {baseline[0]}, SDK {baseline[1]}, Node {baseline[2]}")
        print(f"Extracted: CLI {profile[0]}, SDK {profile[1]}, Node {profile[2]}")
        current, candidate = version_tuple(baseline[0]), version_tuple(profile[0])
        if current[0] != candidate[0]:
            print("WARNING: unsupported Claude major version; keeping baseline", file=sys.stderr)
            return 0
        if candidate <= current:
            print("No update: local Claude is equal to or older than the baseline.")
            return 0
        for name, previous, following in zip(CONSTANTS, baseline, profile):
            print(f"{name}: {previous} -> {following}")
        if not args.apply:
            print("Check only: no files changed.")
            return 0
        updated = original
        for (start, end), value in sorted(zip(spans, profile), reverse=True):
            updated = updated[:start] + value.encode("ascii") + updated[end:]
        atomic_update(baseline_path, original, updated)
        print("Updated internal/app/anthropic_wire.go (three software version constants only).")
        return 0
    except (OSError, ValueError) as error:
        print(f"ERROR: {error}; baseline not updated", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
