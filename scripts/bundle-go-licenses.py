#!/usr/bin/env python3
"""Preserve license notices for the Go packages included in ArtifactFS."""
import json
import pathlib
import shutil
import subprocess
import sys

root = pathlib.Path(__file__).resolve().parents[1]
destination = pathlib.Path(sys.argv[1])
destination.mkdir(parents=True, exist_ok=True)
output = subprocess.check_output(["go", "list", "-deps", "-json", "./cmd/artifact-fs"], cwd=root, text=True)
decoder = json.JSONDecoder()
offset = 0
modules = {}


def copy_notice(source, target):
    if target.exists():
        target.chmod(0o644)
    shutil.copyfile(source, target)

while offset < len(output):
    while offset < len(output) and output[offset].isspace():
        offset += 1
    if offset >= len(output):
        break
    package, consumed = decoder.raw_decode(output[offset:])
    offset += consumed
    module = package.get("Module")
    if module and not module.get("Main"):
        modules[module["Path"]] = module
index = []
for name, module in sorted(modules.items()):
    source = pathlib.Path(module["Dir"])
    notices = sorted(path for path in source.iterdir()
                     if path.is_file() and path.name.lower().startswith(("license", "notice", "copying")))
    if not notices:
        raise SystemExit(f"No license notice found for dependency {name}; review before distribution.")
    folder = destination / name.replace("/", "_")
    folder.mkdir(parents=True, exist_ok=True)
    for notice in notices:
        copy_notice(notice, folder / notice.name)
    index.append({"module": name, "version": module.get("Version"), "notices": [path.name for path in notices]})
go_root = pathlib.Path(subprocess.check_output(["go", "env", "GOROOT"], text=True).strip())
go_license = next((candidate for candidate in (go_root / "LICENSE", go_root.parent / "LICENSE")
                   if candidate.is_file()), None)
if go_license is None:
    raise SystemExit("Could not locate the Go standard library license.")
copy_notice(go_license, destination / "Go-Standard-Library-LICENSE.txt")
for notice in sorted((go_root / "src/vendor").rglob("LICENSE")):
    filename = str(notice.parent.relative_to(go_root / "src/vendor")).replace("/", "_")
    copy_notice(notice, destination / f"Go-vendor-{filename}-LICENSE.txt")
(destination / "dependencies.json").write_text(json.dumps(index, indent=2) + "\n")
