#!/usr/bin/env python3
"""Update the tap from a stable release's checksums (no network or git writes)."""

import argparse
from pathlib import Path
import re


def update_formula(formula: str, tag: str, checksums: str) -> str:
    if not re.fullmatch(r"v\d+\.\d+\.\d+", tag):
        raise ValueError("Homebrew requires a stable vX.Y.Z tag")
    current = re.search(r'^  version "(\d+\.\d+\.\d+)"$', formula, re.M)
    if not current:
        raise ValueError("Missing formula version")
    version = tag[1:]
    if tuple(map(int, version.split("."))) < tuple(map(int, current[1].split("."))):
        raise ValueError("Refusing to downgrade Homebrew formula")
    hashes = {}
    for line in checksums.splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  (\S+)", line)
        if not match:
            raise ValueError(f"Invalid checksum line: {line!r}")
        digest, name = match.groups()
        if name in hashes:
            raise ValueError(f"Duplicate checksum: {name}")
        hashes[name] = digest
    result = formula.replace(current[0], f'  version "{version}"', 1)
    for platform in ("darwin", "linux"):
        for arch in ("arm64", "amd64"):
            name = f"ccload-{platform}-{arch}"
            if name not in hashes:
                raise ValueError(f"Missing checksum: {name}")
            pattern = rf'({re.escape(name)}"\n\s+sha256 ")[0-9a-f]{{64}}(")'
            result, count = re.subn(pattern, lambda m: m[1] + hashes[name] + m[2], result)
            if count != 1:
                raise ValueError(f"Expected one formula URL/checksum for {name}")
    return result


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag")
    parser.add_argument("checksums", type=Path)
    parser.add_argument("formula", type=Path)
    args = parser.parse_args()
    updated = update_formula(args.formula.read_text(), args.tag, args.checksums.read_text())
    args.formula.write_text(updated)
