#!/usr/bin/env python3
"""Install an isolated Android build toolchain under .tools (no global changes)."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tarfile
import urllib.request
import xml.etree.ElementTree as ET
import zipfile

ROOT = Path(__file__).resolve().parent.parent
TOOLS = ROOT / ".tools"
CACHE = TOOLS / "downloads"
SDK = TOOLS / "android-sdk"


def download(url, checksum, algorithm="sha256"):
    target = CACHE / url.rsplit("/", 1)[1]
    def valid():
        if not target.exists():
            return False
        digest = hashlib.new(algorithm)
        with target.open("rb") as src:
            for chunk in iter(lambda: src.read(1024 * 1024), b""):
                digest.update(chunk)
        return digest.hexdigest() == checksum
    if not valid():
        print(f"Downloading {target.name}", flush=True)
        subprocess.run(["curl", "--fail", "--location", "--retry", "2", "--silent",
                        "--show-error", "--output", str(target), url], check=True)
        if not valid():
            raise RuntimeError(f"Checksum mismatch: {target.name}")
    return target


def unzip(archive, destination):
    destination.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(archive) as z:
        for item in z.infolist():
            path = (destination / item.filename).resolve()
            if not path.is_relative_to(destination.resolve()):
                raise RuntimeError("Unsafe archive entry")
            z.extract(item, destination)
            mode = item.external_attr >> 16
            if mode and path.exists():
                path.chmod(mode & 0o777)


def fetch_json(url):
    request = urllib.request.Request(url, headers={"User-Agent": "marionette-bootstrap/1"})
    with urllib.request.urlopen(request, timeout=40) as response:
        return json.load(response)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--device-only", action="store_true",
                        help="use JAVA_HOME if set and skip runtime platform-tools")
    args = parser.parse_args()
    system = platform.system()
    machine = platform.machine()
    if system not in ("Darwin", "Linux"):
        raise SystemExit("Use an existing SDK/JDK with scripts/build-device.sh on this OS.")
    CACHE.mkdir(parents=True, exist_ok=True)
    SDK.mkdir(parents=True, exist_ok=True)
    host = "macosx" if system == "Darwin" else "linux"
    java_home = os.environ.get("JAVA_HOME") if args.device_only else None
    jdk = Path(java_home) if java_home else TOOLS / "jdk"
    if not (jdk / "bin" / "javac").exists():
        if java_home:
            raise SystemExit(f"JAVA_HOME does not contain bin/javac: {jdk}")
        os_name = "macos" if system == "Darwin" else "linux"
        arch = "aarch64" if machine in ("arm64", "aarch64") else "x86"
        packages = fetch_json("https://api.azul.com/metadata/v1/zulu/packages/"
                              f"?java_version=17&os={os_name}&arch={arch}"
                              "&archive_type=tar.gz&java_package_type=jdk&latest=true&release_status=ga")
        package = next(p for p in packages if "-ca-jdk" in p["name"])
        details = fetch_json("https://api.azul.com/metadata/v1/zulu/packages/" + package["package_uuid"])
        archive = download(details["download_url"], details["sha256_hash"])
        extracted = TOOLS / "jdk-extract"
        extracted.mkdir(exist_ok=True)
        with tarfile.open(archive) as t:
            t.extractall(extracted, filter="data")
        candidates = list(extracted.glob("*/Contents/Home")) + list(extracted.glob("*/bin/javac"))
        if not candidates:
            raise RuntimeError("JDK home missing from archive")
        home = candidates[0]
        if home.name == "javac":
            home = home.parent.parent
        home.rename(jdk)
        print(f"JDK ready: {jdk}", flush=True)
    needed = {
        "platforms;android-34": SDK / "platforms" / "android-34" / "android.jar",
        "build-tools;34.0.0": SDK / "build-tools" / "34.0.0" / "lib" / "d8.jar",
    }
    if not args.device_only:
        needed["platform-tools"] = SDK / "platform-tools" / "adb"
    if any(not marker.exists() for marker in needed.values()):
        print("Reading Google's Android SDK package index", flush=True)
        with urllib.request.urlopen("https://dl.google.com/android/repository/repository2-1.xml", timeout=40) as r:
            repository = ET.fromstring(r.read())
        for package_name, marker in needed.items():
            if marker.exists():
                continue
            package = next(p for p in repository.findall("remotePackage") if p.get("path") == package_name)
            artifact = next(a.find("complete") for a in package.findall("./archives/archive")
                            if a.findtext("host-os") in (None, host))
            checksum = artifact.find("checksum")
            archive = download("https://dl.google.com/android/repository/" + artifact.findtext("url"),
                               checksum.text, checksum.get("type") or "sha1")
            staging = TOOLS / ("extract-" + package_name.replace(";", "-"))
            unzip(archive, staging)
            if package_name == "platform-tools":
                source = staging / "platform-tools"
                target = SDK / "platform-tools"
            else:
                source = next(p for p in staging.iterdir() if p.is_dir())
                target = marker.parent if package_name.startswith("platforms;") else marker.parent.parent
            target.parent.mkdir(parents=True, exist_ok=True)
            source.rename(target)
            if not marker.exists():
                raise RuntimeError(f"Expected SDK artifact missing: {marker}")
            print(f"Ready: {package_name}", flush=True)
    print("Toolchain ready. Run: make device", flush=True)


if __name__ == "__main__":
    main()
