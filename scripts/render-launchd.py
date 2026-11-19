#!/usr/bin/env python3
"""Write a reviewable per-user launchd configuration; never register or start it."""

import argparse
import os
from pathlib import Path
import plistlib


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("component", choices=("agent", "backend", "web", "runner"))
    parser.add_argument("--install-dir", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path,
                        help="new plist file; existing files are never overwritten")
    args = parser.parse_args()
    root = args.install_dir.expanduser().resolve(strict=True)
    if args.component == "agent" and (root / "start-agent.sh").is_file():
        command = ["/bin/bash", str(root / "start-agent.sh")]
    elif args.component == "agent":
        command = ["/bin/bash", str(root / "scripts/start-agent.sh")]
    else:
        command = ["/bin/bash", str(root / "scripts/start-service.sh"), args.component]
    if not Path(command[1]).is_file():
        parser.error(f"startup script does not exist: {command[1]}")
    logs = root / ".marionette/logs"
    output = args.output.expanduser().absolute()
    if output.exists():
        parser.error(f"refusing to overwrite {output}")
    os.umask(0o077)
    config = {
        "Label": f"io.marionette.{args.component}",
        "ProgramArguments": command,
        "WorkingDirectory": str(root),
        "EnvironmentVariables": {
            "PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
        },
        "RunAtLoad": True,
        "KeepAlive": True,
        "ThrottleInterval": 10,
        "ExitTimeOut": 60,
        "Umask": 0o077,
        "StandardOutPath": str(logs / f"{args.component}.log"),
        "StandardErrorPath": str(logs / f"{args.component}.log"),
    }
    logs.mkdir(mode=0o700, parents=True, exist_ok=True)
    output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    # O_EXCL also protects existing configs against races or dangling symlinks.
    descriptor = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as stream:
        plistlib.dump(config, stream, sort_keys=False)
    print(output)


if __name__ == "__main__":
    main()
