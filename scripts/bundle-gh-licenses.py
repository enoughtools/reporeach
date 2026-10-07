#!/usr/bin/env python3
"""Bundle notices from the exact modules embedded in an official gh binary.

Module downloads run outside the application module with checksum-database
verification enabled. Every source ZIP is also independently hashed using Go's
h1 algorithm and checked against the binary's build information before copying.
No root go.mod/go.sum or application compiler settings are changed.
Notice bytes are unchanged, but every copied resource ends in .txt so notices
whose upstream names end in .go cannot become buildable application packages.
"""

import argparse
import base64
import concurrent.futures
import hashlib
import json
import os
import pathlib
import re
import shutil
import subprocess
import tempfile
import zipfile


NOTICE_NAME = re.compile(r"^(?:licen[cs]e|notice|copying|copyright)(?:$|[._ -])", re.I)
VERIFICATION = "Go checksum database plus independent module ZIP h1 verification"


def resource_path(source_path):
    """Keep the source path reversible while making every notice inert."""
    return source_path.with_name(source_path.name + ".txt")


def publish_notices(staging, destination):
    """Replace only output owned by this collector, including stale .go files."""
    owned_names = ("modules", "go-standard-library", "dependencies.json")
    existing = [destination / name for name in owned_names
                if (destination / name).exists() or (destination / name).is_symlink()]
    if existing:
        manifest_path = destination / "dependencies.json"
        if not manifest_path.is_file() or manifest_path.is_symlink():
            raise RuntimeError(f"Refusing to replace unrecognized notice output in {destination}.")
        previous = json.loads(manifest_path.read_text())
        if (previous.get("verification") != VERIFICATION
                or previous.get("checksum_database") != "sum.golang.org"
                or not isinstance(previous.get("modules"), list)
                or not isinstance(previous.get("go_standard_library"), list)):
            raise RuntimeError(f"Refusing to replace unrecognized notice output in {destination}.")
    destination.mkdir(parents=True, exist_ok=True)
    for target in existing:
        if target.is_dir() and not target.is_symlink():
            shutil.rmtree(target)
        else:
            target.unlink()
    shutil.copytree(staging, destination, dirs_exist_ok=True)


def run_go(arguments, cwd, environment):
    result = subprocess.run(["go", *arguments], cwd=cwd, env=environment,
                            text=True, capture_output=True, check=False)
    if result.returncode:
        raise RuntimeError(f"go {' '.join(arguments)} failed:\n{result.stderr.strip()}")
    return result.stdout


def binary_modules(binary, cwd, environment):
    output = run_go(["version", "-m", str(binary)], cwd, environment)
    toolchain = output.splitlines()[0].rsplit(": ", 1)[-1]
    if not re.fullmatch(r"go\d+\.\d+\.\d+", toolchain):
        raise RuntimeError(f"Unrecognized binary toolchain: {toolchain}")
    modules = []
    for line in output.splitlines()[1:]:
        fields = line.strip().split()
        if fields and fields[0] in ("mod", "dep"):
            if len(fields) < 3 or fields[2] == "(devel)":
                raise RuntimeError(f"Unversioned module cannot be audited: {line.strip()}")
            checksum = fields[3] if len(fields) > 3 else None
            if fields[0] == "dep" and not (checksum or "").startswith("h1:"):
                raise RuntimeError(f"Dependency lacks h1 checksum: {line.strip()}")
            modules.append({"module": fields[1], "version": fields[2],
                            "binary_sum": checksum, "main": fields[0] == "mod"})
        elif fields and fields[0] == "=>":
            raise RuntimeError("Replaced modules require a separate distribution audit.")
    if not modules or sum(item["main"] for item in modules) != 1:
        raise RuntimeError("Binary build information does not identify one main module.")
    return toolchain, modules


def zip_h1(archive):
    """Equivalent to golang.org/x/mod/sumdb/dirhash.HashZip (Hash1)."""
    names = archive.namelist()
    if len(names) != len(set(names)):
        raise RuntimeError("Module ZIP contains duplicate paths.")
    digest = hashlib.sha256()
    for name in sorted(names):
        file_hash = hashlib.sha256()
        with archive.open(name) as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                file_hash.update(chunk)
        digest.update(f"{file_hash.hexdigest()}  {name}\n".encode())
    return "h1:" + base64.b64encode(digest.digest()).decode()


def audit_module(module, staging, cwd, environment):
    identifier = f"{module['module']}@{module['version']}"
    downloaded = json.loads(run_go(["mod", "download", "-json", identifier], cwd, environment))
    if downloaded.get("Error") or not downloaded.get("Sum") or not downloaded.get("Zip"):
        raise RuntimeError(f"Incomplete module download: {identifier}: {downloaded.get('Error', '')}")
    expected = module["binary_sum"] or downloaded["Sum"]
    if downloaded["Sum"] != expected:
        raise RuntimeError(f"Binary/source checksum mismatch: {identifier}")
    notices = []
    prefix = identifier + "/"
    module_folder = pathlib.Path("modules") / module["module"] / module["version"]
    with zipfile.ZipFile(downloaded["Zip"]) as archive:
        if zip_h1(archive) != expected:
            raise RuntimeError(f"Source ZIP h1 checksum mismatch: {identifier}")
        for name in sorted(archive.namelist()):
            if not name.startswith(prefix):
                raise RuntimeError(f"Unexpected source ZIP path: {name}")
            relative = pathlib.PurePosixPath(name[len(prefix):])
            if relative.is_absolute() or ".." in relative.parts:
                raise RuntimeError(f"Unsafe source ZIP path: {name}")
            if name.endswith("/") or not NOTICE_NAME.match(relative.name):
                continue
            bundled = resource_path(relative)
            target = staging / module_folder / bundled
            target.parent.mkdir(parents=True, exist_ok=True)
            with archive.open(name) as source, target.open("wb") as destination:
                shutil.copyfileobj(source, destination)
            notices.append({"source_path": str(relative), "path": str(bundled),
                            "sha256": hashlib.sha256(target.read_bytes()).hexdigest()})
    if not notices:
        raise RuntimeError(f"No license/notice/copying/copyright file found: {identifier}; review before distribution.")
    return {**module, "sum": expected, "directory": str(module_folder), "notices": notices}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=pathlib.Path)
    parser.add_argument("--destination", required=True, type=pathlib.Path,
                        help="Dedicated notice directory; existing collector output is replaced")
    arguments = parser.parse_args()
    binary = arguments.binary.resolve(strict=True)
    destination = arguments.destination.resolve()
    root = pathlib.Path(__file__).resolve().parents[1]
    cache = root / "build/vendor/gh/license-audit"
    cache.mkdir(parents=True, exist_ok=True)
    environment = os.environ.copy()
    environment.update({"GOWORK": "off", "GOFLAGS": "", "GOPROXY": "https://proxy.golang.org",
                        "GOSUMDB": "sum.golang.org", "GONOSUMDB": "", "GOPRIVATE": "",
                        "GONOPROXY": "", "GOTOOLCHAIN": "local"})
    with tempfile.TemporaryDirectory(prefix="audit-", dir=cache) as scratch:
        cwd = pathlib.Path(scratch)
        toolchain, modules = binary_modules(binary, cwd, environment)
        environment["GOTOOLCHAIN"] = toolchain
        # Fetch the exact compiler once before concurrent downloads need it.
        go_root = pathlib.Path(run_go(["env", "GOROOT"], cwd, environment).strip())
        staging = cwd / "notices"
        staging.mkdir()
        index, failures = [], []
        with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
            futures = {pool.submit(audit_module, item, staging, cwd, environment): item for item in modules}
            for future in concurrent.futures.as_completed(futures):
                try:
                    index.append(future.result())
                except Exception as error:
                    failures.append(str(error))
        if failures:
            raise RuntimeError("GitHub CLI notice audit failed:\n" + "\n".join(sorted(failures)))
        standard_library = []
        sources = [go_root / "LICENSE"]
        sources.extend(path for path in (go_root / "src/vendor").rglob("*")
                       if path.is_file() and NOTICE_NAME.match(path.name))
        for source in sorted(sources):
            if not source.is_file():
                raise RuntimeError(f"Missing Go standard-library license: {source}")
            relative = source.relative_to(go_root)
            bundled = resource_path(relative)
            target = staging / "go-standard-library" / bundled
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source, target)
            standard_library.append({"source_path": str(relative), "path": str(bundled),
                                     "sha256": hashlib.sha256(target.read_bytes()).hexdigest()})
        manifest = {"binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                    "binary_hash_scope": "Official input executable before EnoughRepos code signing",
                    "toolchain": toolchain, "checksum_database": "sum.golang.org",
                    "verification": VERIFICATION,
                    "notice_resource_format": "Exact upstream bytes with .txt suffix; source_path retains original relative path",
                    "dependency_count": sum(not item["main"] for item in index),
                    "modules": sorted(index, key=lambda item: item["module"]),
                    "go_standard_library": standard_library}
        (staging / "dependencies.json").write_text(json.dumps(manifest, indent=2) + "\n")
        publish_notices(staging, destination)
        print(f"Preserved {len(index)} module notice sets ({manifest['dependency_count']} dependencies) "
              f"and {toolchain} standard-library notices in {destination}")


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, ValueError, zipfile.BadZipFile) as error:
        raise SystemExit(str(error)) from None
