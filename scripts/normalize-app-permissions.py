#!/usr/bin/env python3
"""Normalize a staged app before signing, or verify its distribution modes."""
import argparse
import os
import pathlib
import stat
import sys


def inventory(app):
    root = app.lstat()
    if not stat.S_ISDIR(root.st_mode):
        raise ValueError("The app bundle must be a real directory.")
    entries = [(app, root, 0o755)]
    pending = [app]
    while pending:
        directory = pending.pop()
        with os.scandir(directory) as children:
            for child in children:
                info = child.stat(follow_symlinks=False)
                path = pathlib.Path(child.path)
                if stat.S_ISLNK(info.st_mode):
                    continue
                if stat.S_ISDIR(info.st_mode):
                    entries.append((path, info, 0o755))
                    pending.append(path)
                elif stat.S_ISREG(info.st_mode):
                    mode = 0o755 if info.st_mode & 0o111 else 0o644
                    entries.append((path, info, mode))
                else:
                    raise ValueError("The app bundle contains an unsupported file type.")
    return entries


def normalize(app, check=False):
    # Inspect the complete bundle before changing any mode. Links are opaque;
    # neither this scan nor the subsequent descriptor opens follow a link leaf.
    entries = inventory(app)
    for path, expected, mode in entries:
        flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
        if stat.S_ISDIR(expected.st_mode):
            flags |= os.O_DIRECTORY
        descriptor = os.open(path, flags)
        try:
            current = os.fstat(descriptor)
            if (current.st_dev, current.st_ino, stat.S_IFMT(current.st_mode)) != (
                    expected.st_dev, expected.st_ino, stat.S_IFMT(expected.st_mode)):
                raise ValueError("The app bundle changed during permission inspection.")
            if check:
                if stat.S_IMODE(current.st_mode) != mode:
                    raise ValueError("The app bundle does not have public distribution permissions.")
            else:
                os.fchmod(descriptor, mode)
        finally:
            os.close(descriptor)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--app", type=pathlib.Path, required=True)
    parser.add_argument("--check", action="store_true", help="Verify without changing permissions")
    args = parser.parse_args()
    if not args.app.is_absolute():
        parser.error("--app must be an absolute path")
    try:
        normalize(args.app, check=args.check)
    except (OSError, ValueError) as error:
        print(str(error) if isinstance(error, ValueError) else "App bundle permission inspection failed.", file=sys.stderr)
        return 1
    print("App bundle distribution permissions verified." if args.check else "App bundle distribution permissions normalized.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
