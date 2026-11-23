#!/usr/bin/env python3
"""Build the standalone macOS / Windows CLI release archives."""

import argparse
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import zipfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    output = root / "build" / "cli-release"
    output.mkdir(parents=True, exist_ok=True)
    commit = subprocess.check_output(
        ["git", "rev-parse", "HEAD"], cwd=root, text=True
    ).strip()
    checksums = []
    with tempfile.TemporaryDirectory(prefix="cli-package-", dir=output) as staging:
        staging = Path(staging)
        for system, arch in [("darwin", "arm64"), ("darwin", "amd64"), ("windows", "amd64")]:
            name = f"marionette-cli-{system}-{arch}"
            package = staging / name
            package.mkdir()
            binary = package / ("marionette.exe" if system == "windows" else "marionette")
            subprocess.run(
                [
                    "go", "build", "-trimpath",
                    "-ldflags", f"-s -w -X main.version={args.version} -X main.commit={commit}",
                    "-o", str(binary), "./cmd/marionette",
                ],
                cwd=root,
                env={**os.environ, "CGO_ENABLED": "0", "GOOS": system, "GOARCH": arch},
                check=True,
            )
            binary.chmod(0o755)
            (package / "README.md").write_bytes((root / "docs" / "CLI_INSTALL.md").read_bytes())
            (package / "VERSION").write_text(f"{args.version}\n{commit}\n", encoding="utf-8")
            archive = output / f"{name}.zip"
            with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as bundle:
                for item in sorted(package.iterdir()):
                    bundle.write(item, arcname=f"{name}/{item.name}")
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()
            checksums.append(f"{digest}  {archive.name}\n")
            print(archive.relative_to(root), flush=True)
    (output / "SHA256SUMS").write_text("".join(checksums), encoding="utf-8")


if __name__ == "__main__":
    main()
