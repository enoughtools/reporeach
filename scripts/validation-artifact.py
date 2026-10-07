#!/usr/bin/env python3
"""Export an unprovisioned CI app for local signing and mounted validation."""
import argparse
import hashlib
import json
import pathlib
import re
import subprocess
import sys
import tempfile


def require(condition, message):
    if not condition:
        raise ValueError(message)


def run(*arguments):
    return subprocess.check_output(arguments, stderr=subprocess.STDOUT).decode().strip()


def source_identity(path):
    source = json.loads(path.read_text())
    require(source.get("url") == "https://github.com/enoughtools/reporeach", "Unexpected validation source repository")
    require(source.get("dirty") is False, "Local validation exports require a committed, clean source checkout")
    require(re.fullmatch(r"[0-9a-f]{40}", source.get("revision", "")), "Invalid validation source revision")
    require(re.fullmatch(r"[0-9a-f]{64}", source.get("contentSha256", "")), "Missing source content fingerprint")
    return source


def digest(path):
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(block)
    return result.hexdigest()


def export(args):
    require(sys.platform == "darwin", "Export local validation apps on the macOS build runner")
    source = source_identity(args.source_file)
    module = args.app / "Contents/Extensions/RepoReachFSKit.appex"
    require(module.is_dir(), "Local validation app must include its FSKit module")
    require(not (module / "Contents/embedded.provisionprofile").exists(), "Local validation exports must not contain a provisioning profile")
    for helper in ("artifact-fs", "gh"):
        executable = args.app / "Contents/Helpers" / helper
        require(executable.is_file(), f"Bundled {helper} helper is missing")
        require(run("lipo", "-archs", str(executable)).split() == [args.arch], f"Bundled {helper} architecture does not match the app")
    args.output.mkdir(parents=True, exist_ok=True)
    name = f"EnoughRepos-local-validation-{args.arch}"
    archive = args.output / f"{name}.zip"
    require(not archive.exists(), "Refusing to overwrite an existing local validation archive")
    warning = (
        "EnoughRepos local validation artifact — not a release.\n"
        "This app has no authorized FSKit provisioning profile or Developer ID signature.\n"
        "Unprovisioned local signing may require repeated user approval.\n"
        "Production distribution requires a matching FSKit profile and Developer ID signing.\n"
        "Keep signing credentials on the local Mac; never upload them to GitHub Actions.\n"
        "Use an isolated test state directory and a disposable empty mount folder.\n"
        "Do not replace an existing installed app or its data with this validation product.\n"
        "A successful build, registration or signature does not establish a working filesystem mount.\n"
    )
    (args.app / "Contents/Resources/LocalValidation.txt").write_text(warning)
    (args.output / "LOCAL-VALIDATION.txt").write_text(warning)
    with tempfile.TemporaryDirectory(prefix="reporeach-validation-", dir=args.output) as folder:
        temporary = pathlib.Path(folder) / archive.name
        run("ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", str(args.app), str(temporary))
        temporary.replace(archive)
    document = {
        "purpose": "local-fskit-validation",
        "distribution": False,
        "source": source,
        "architecture": args.arch,
        "filesystemBackend": "fskit",
        "minimumMountMacOS": "26.0",
        "localSigningRequired": True,
        "extensionActivationAuthorized": False,
        "mountedValidationPassed": False,
        "toolchain": {"xcode": run("xcodebuild", "-version"), "macOSSDK": run("xcrun", "--sdk", "macosx", "--show-sdk-version"), "go": run("go", "version")},
        "artifact": {"filename": archive.name, "bytes": archive.stat().st_size, "sha256": digest(archive)},
    }
    (args.output / f"{name}.json").write_text(json.dumps(document, indent=2) + "\n")
    (args.output / "SHA256SUMS").write_text(f"{document['artifact']['sha256']}  {archive.name}\n")
    print(f"Exported unprovisioned local-validation app: {archive}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--app", type=pathlib.Path, required=True)
    parser.add_argument("--arch", choices=("arm64", "x86_64"), required=True)
    parser.add_argument("--source-file", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    try:
        export(args)
    except (ValueError, OSError, subprocess.CalledProcessError) as failure:
        raise SystemExit(f"Local validation export failed: {failure}")


if __name__ == "__main__":
    main()
