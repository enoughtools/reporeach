#!/usr/bin/env python3
"""Generate download metadata from verified release archives, then stage it."""
import argparse
import datetime
import hashlib
import json
import pathlib
import shutil
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[1]
MAX_ASSET_BYTES = 25 * 1024 * 1024


def git(*args):
    return subprocess.check_output(["git", "-C", str(ROOT), *args], text=True).strip()


def digest(path):
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(block)
    return result.hexdigest()


def source_identity():
    files = subprocess.check_output(["git", "-C", str(ROOT), "ls-files", "-z", "--cached", "--others", "--exclude-standard"])
    fingerprint = hashlib.sha256()
    for relative in sorted(set(files.split(b"\0")) - {b""}):
        path = ROOT / relative.decode("utf-8")
        if path.is_file():
            fingerprint.update(relative + b"\0" + bytes.fromhex(digest(path)))
    return {"url": "https://github.com/enoughtools/reporeach", "revision": git("rev-parse", "HEAD"),
            "dirty": bool(git("status", "--porcelain")), "contentSha256": fingerprint.hexdigest()}


def source(args):
    args.output.write_text(json.dumps(source_identity(), indent=2) + "\n")


def record(args):
    args.directory.mkdir(parents=True, exist_ok=True)
    artifacts = []
    for extension in ("dmg", "zip"):
        filename = f"RepoReach-{args.version}-macOS-{args.arch}.{extension}"
        archive = args.directory / filename
        if not archive.is_file():
            raise SystemExit(f"Missing release artifact: {archive}")
        artifacts.append({
            "architecture": args.arch, "format": extension, "filename": filename,
            "url": f"https://reporeach.reb.run/releases/{args.version}/{filename}",
            "sha256": digest(archive), "bytes": archive.stat().st_size,
            "signature": args.signature, "notarized": args.notarized == "true",
        })
    toolchain = {
        "go": subprocess.check_output(["go", "version"], text=True).strip(),
        "xcode": subprocess.check_output(["xcodebuild", "-version"], text=True).strip(),
        "xcodegen": subprocess.check_output(["xcodegen", "--version"], text=True).strip(),
    }
    metadata = {"version": args.version, "filesystemBackend": args.backend, "source": json.loads(args.source_file.read_text()),
                "architecture": args.arch, "toolchain": toolchain, "artifacts": artifacts}
    if args.backend == "fskit":
        if args.signature != "developer-id" or args.fskit_profile is None or not args.fskit_profile.is_file():
            raise SystemExit("FSKit distribution metadata requires Developer ID signing and the actual embedded module profile.")
        metadata["filesystemModule"] = {
            "bundleIdentifier": "com.enoughtools.reporeach.fskit",
            "embeddedProfileSha256": digest(args.fskit_profile),
        }
    (args.directory / f".metadata-{args.arch}.json").write_text(json.dumps(metadata, indent=2) + "\n")


def manifest(args):
    artifacts = []
    builds = []
    backends = set()
    identity = source_identity()
    for metadata in sorted(args.directory.glob(".metadata-*.json")):
        build = json.loads(metadata.read_text())
        if isinstance(build, list):
            raise SystemExit("Artifacts lack per-build source provenance. Rebuild with the current packaging script.")
        if build["version"] != args.version or build["source"]["contentSha256"] != identity["contentSha256"]:
            raise SystemExit("Source changed since packaging, or architecture versions differ. Rebuild before publishing.")
        artifacts.extend(build["artifacts"])
        backends.add(build.get("filesystemBackend", "macfuse"))
        built = {"architecture": build["architecture"], "source": build["source"], "toolchain": build["toolchain"]}
        if "filesystemModule" in build:
            built["filesystemModule"] = build["filesystemModule"]
        builds.append(built)
    if not artifacts:
        raise SystemExit("No packaged artifacts found. Run build-macos.sh first.")
    if len(backends) != 1:
        raise SystemExit("Architecture packages use different filesystem backends. Rebuild before publishing.")
    backend = backends.pop()
    setup_document = "native-fskit.md" if backend == "fskit" else "platform-setup.md"
    setup_url = f"{identity['url']}/blob/{identity['revision']}/docs/reporeach/{setup_document}"
    if backend == "fskit":
        modules = [build.get("filesystemModule") for build in builds]
        if not all(module and module.get("embeddedProfileSha256") for module in modules) or not all(module == modules[0] for module in modules):
            raise SystemExit("FSKit builds must contain the same authorized module provisioning profile.")
        if any(item["signature"] != "developer-id" for item in artifacts):
            raise SystemExit("FSKit release archives must use Developer ID signing.")
    for item in artifacts:
        archive = args.directory / item["filename"]
        if digest(archive) != item["sha256"] or archive.stat().st_size != item["bytes"]:
            raise SystemExit(f"Artifact changed after packaging: {archive}")
    document = {
        "product": "RepoReach", "version": args.version,
        "releasedAt": datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z"),
        "source": identity,
        "minimumMacOS": "13.0", "filesystemBackend": backend,
        "minimumMountMacOS": "26.0" if backend == "fskit" else "13.0",
        "requirements": {
            "macFUSE": True, "macFUSEBackend": "kernel", "macFUSEURL": "https://macfuse.io/",
            "git": True,
            "setupURL": setup_url,
            "appleSiliconApproval": "The kernel backend requires approval in System Settings and may require Reduced Security and user-managed kernel extensions in macOS Recovery.",
        },
        "githubCLI": {"version": "2.102.0"}, "builds": builds, "artifacts": artifacts,
    }
    if backend == "fskit":
        document["requirements"] = {
            "macFUSE": False, "fsKit": True, "fsKitModuleBundled": True,
            "git": True,
            "setupURL": setup_url,
            "extensionEnablement": "Enable the bundled RepoReach FSKit extension in System Settings. Virtual mounts require macOS 26 or later.",
        }
    (args.directory / "release.json").write_text(json.dumps(document, indent=2) + "\n")
    (args.directory / "SHA256SUMS").write_text("".join(f"{item['sha256']}  {item['filename']}\n" for item in artifacts))
    return document


def stage(args):
    document = manifest(args)
    for item in document["artifacts"]:
        if item["bytes"] > MAX_ASSET_BYTES:
            raise SystemExit(f"{item['filename']} exceeds Cloudflare's 25 MiB asset limit. Host it in object storage.")
    destination = args.site_public / "releases" / args.version
    destination.mkdir(parents=True, exist_ok=True)
    for filename in [item["filename"] for item in document["artifacts"]] + ["release.json", "SHA256SUMS"]:
        shutil.copy2(args.directory / filename, destination / filename)
    shutil.copy2(args.directory / "release.json", destination.parent / "latest.json")
    print(f"Staged {len(document['artifacts'])} verified artifacts in {destination}")


parser = argparse.ArgumentParser()
commands = parser.add_subparsers(dest="command", required=True)
source_command = commands.add_parser("source")
source_command.add_argument("--output", type=pathlib.Path, required=True)
for name in ("record", "manifest", "stage"):
    command = commands.add_parser(name)
    command.add_argument("--directory", type=pathlib.Path, required=True)
    command.add_argument("--version", required=True)
    if name == "record":
        command.add_argument("--arch", choices=["arm64", "x86_64"], required=True)
        command.add_argument("--backend", choices=["fskit", "macfuse"], default="macfuse")
        command.add_argument("--fskit-profile", type=pathlib.Path)
        command.add_argument("--signature", choices=["developer-id", "ad-hoc"], required=True)
        command.add_argument("--notarized", choices=["true", "false"], required=True)
        command.add_argument("--source-file", type=pathlib.Path, required=True)
    if name == "stage":
        command.add_argument("--site-public", type=pathlib.Path, default=ROOT / "site/public")
args = parser.parse_args()
{"source": source, "record": record, "manifest": manifest, "stage": stage}[args.command](args)
