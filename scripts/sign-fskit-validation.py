#!/usr/bin/env python3
"""Sign a verified CI validation app locally without compiling or exporting keys.

This creates only an isolated test app. Signing does not enable its extension,
prove a working mount, notarize it, or authorize a production release.
"""
import argparse
import base64
import datetime
import hashlib
import importlib.util
import io
import json
import os
import pathlib
import plistlib
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import unicodedata
import zipfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
REPOSITORY = "enoughtools/reporeach"
SOURCE_URL = f"https://github.com/{REPOSITORY}"
MAX_ARCHIVE = 256 * 1024 * 1024
MAX_EXPANDED = 512 * 1024 * 1024
MAX_MEMBERS = 50000
MARKER = "RepoReach local validation artifact — not a release.\n"


def load_script(name):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), ROOT / "scripts" / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


packaging = load_script("validate-fskit-bundle")
validation = load_script("validation-artifact")
require = packaging.require
digest = validation.digest


def run(*arguments):
    result = subprocess.run(arguments, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    require(result.returncode == 0, f"{pathlib.Path(arguments[0]).name} failed during local validation signing")
    return result.stdout


def committed_fingerprint(revision):
    # Read the committed tree, without extracting it or following source links.
    # This is the release-manifest.py fingerprint for ordinary tracked files.
    data = run("git", "-C", str(ROOT), "archive", "--format=tar", revision)
    files = []
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as archive:
        for member in archive:
            if member.isdir():
                continue
            require(member.isfile(), "Source tree contains an unsupported non-file entry")
            relative = member.name.encode("utf-8")
            files.append((relative, hashlib.sha256(archive.extractfile(member).read()).digest()))
    fingerprint = hashlib.sha256()
    for relative, content in sorted(files):
        fingerprint.update(relative + b"\0" + content)
    return fingerprint.hexdigest()


def verify_source(args, document):
    require(re.fullmatch(r"[0-9a-f]{40}", args.source_revision) is not None, "Expected source revision must be a full Git commit ID")
    for value in (args.source_sha256, args.archive_sha256):
        require(re.fullmatch(r"[0-9a-f]{64}", value) is not None, "Expected fingerprints must be full SHA-256 values")
    source = document.get("source", {})
    require(source == {"url": SOURCE_URL, "revision": args.source_revision, "dirty": False, "contentSha256": args.source_sha256}, "Artifact source identity differs from the expected clean CI source")
    remote = run("git", "-C", str(ROOT), "remote", "get-url", "origin").decode().strip()
    require(remote in (SOURCE_URL, SOURCE_URL + ".git", f"git@github.com:{REPOSITORY}.git", f"ssh://git@github.com/{REPOSITORY}.git"), "Signing utility checkout has an unexpected origin")
    require(not run("git", "-C", str(ROOT), "status", "--porcelain").strip(), "Commit or discard source changes before local signing")
    resolved = run("git", "-C", str(ROOT), "rev-parse", args.source_revision + "^{commit}").decode().strip()
    require(resolved == args.source_revision, "Expected CI revision is not a locally available commit")
    require(committed_fingerprint(args.source_revision) == args.source_sha256, "Committed source fingerprint differs from the CI artifact")
    utility_revision = run("git", "-C", str(ROOT), "rev-parse", "HEAD").decode().strip()
    return {"url": SOURCE_URL, "revision": utility_revision, "dirty": False, "contentSha256": committed_fingerprint(utility_revision)}


def verify_workflow(args):
    require(args.workflow_run > 0, "Expected workflow run must be a positive ID")
    run_info = json.loads(run("gh", "api", f"repos/{REPOSITORY}/actions/runs/{args.workflow_run}"))
    require(run_info.get("repository", {}).get("full_name") == REPOSITORY, "Workflow belongs to a different repository")
    require(run_info.get("head_sha") == args.source_revision, "Workflow source revision differs from the expected artifact")
    require(run_info.get("path") == ".github/workflows/reporeach.yml" and run_info.get("event") == "workflow_dispatch", "Unexpected validation workflow")
    require(run_info.get("status") == "completed" and run_info.get("conclusion") == "success", "Validation workflow has not completed successfully")
    artifacts = json.loads(run("gh", "api", f"repos/{REPOSITORY}/actions/runs/{args.workflow_run}/artifacts?per_page=100")).get("artifacts", [])
    matching = [item for item in artifacts if item.get("name") == f"fskit-local-validation-{args.arch}"]
    require(len(matching) == 1 and matching[0].get("expired") is False, "Expected active validation artifact is missing or ambiguous")
    require(matching[0].get("workflow_run", {}).get("head_sha") == args.source_revision, "Validation artifact belongs to a different source revision")
    return {"url": f"{SOURCE_URL}/actions/runs/{args.workflow_run}", "artifactId": matching[0]["id"]}


def verify_metadata(args):
    require(args.metadata.is_file() and args.metadata.stat().st_size <= 64 * 1024, "Validation metadata is missing or too large")
    document = json.loads(args.metadata.read_text())
    require(isinstance(document, dict), "Validation metadata must be an object")
    require(document.get("purpose") == "local-fskit-validation" and document.get("distribution") is False, "Input must be an explicit local validation artifact")
    require(document.get("architecture") == args.arch and document.get("filesystemBackend") == "fskit", "Validation architecture or backend differs")
    require(document.get("localSigningRequired") is True and document.get("extensionActivationAuthorized") is False and document.get("mountedValidationPassed") is False, "Input makes unexpected activation or mounted-validation claims")
    artifact = document.get("artifact", {})
    require(isinstance(artifact, dict), "Validation archive metadata must be an object")
    expected_name = f"RepoReach-local-validation-{args.arch}.zip"
    require(args.archive.name == expected_name and artifact.get("filename") == expected_name, "Unexpected validation archive filename")
    require(args.archive.is_file() and 0 < args.archive.stat().st_size <= MAX_ARCHIVE, "Validation archive is missing or too large")
    require(type(artifact.get("bytes")) is int and artifact["bytes"] == args.archive.stat().st_size, "Archive size differs from its metadata")
    require(artifact.get("sha256") == args.archive_sha256 and digest(args.archive) == args.archive_sha256, "Archive differs from the independently expected SHA-256")
    return document


def archive_entries(archive):
    # No general-purpose archive extractor: reject links, devices, traversal,
    # collisions on case-insensitive macOS, and bounded expansion before writing.
    members = archive.infolist()
    require(0 < len(members) <= MAX_MEMBERS and sum(item.file_size for item in members) <= MAX_EXPANDED, "ZIP exceeds local validation extraction limits")
    seen, spellings, entries = {}, {}, []
    for item in members:
        name = item.orig_filename
        require("\0" not in name and "\\" not in name and not name.startswith("/"), "Unsafe ZIP member path")
        parts = name.rstrip("/").split("/")
        require(all(part not in ("", ".", "..") for part in parts), "Unsafe ZIP member path")
        require(parts[0] in ("RepoReach.app", "__MACOSX"), "ZIP contains an unexpected top-level entry")
        key = unicodedata.normalize("NFC", "/".join(parts)).casefold()
        require(key not in seen, "ZIP contains duplicate or case-colliding paths")
        for index in range(1, len(parts) + 1):
            spelling = "/".join(parts[:index])
            parent = unicodedata.normalize("NFC", spelling).casefold()
            require(parent not in spellings or spellings[parent] == spelling, "ZIP contains case-colliding parent paths")
            spellings[parent] = spelling
        mode = item.external_attr >> 16
        kind = stat.S_IFMT(mode)
        require(kind in (0, stat.S_IFREG, stat.S_IFDIR) and not mode & 0o7000, "ZIP links, special files and privileged modes are forbidden")
        require(not (item.flag_bits & 1) and item.compress_type in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED), "Unsupported encrypted or compressed ZIP member")
        require(item.is_dir() == (kind == stat.S_IFDIR) or kind == 0 or (not item.is_dir() and kind == stat.S_IFREG), "ZIP member type is inconsistent")
        seen[key] = item.is_dir()
        entries.append((item, parts, mode))
    for _, parts, _ in entries:
        for index in range(1, len(parts)):
            parent = unicodedata.normalize("NFC", "/".join(parts[:index])).casefold()
            require(parent not in seen or seen[parent], "ZIP file is also used as a parent directory")
    return entries


def extract_app(archive_path, destination):
    with zipfile.ZipFile(archive_path) as archive:
        for item, parts, mode in archive_entries(archive):
            if parts[0] == "__MACOSX":
                continue  # AppleDouble metadata is not executable app content.
            target = destination.joinpath(*parts)
            if item.is_dir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with archive.open(item) as source, target.open("xb") as output:
                    shutil.copyfileobj(source, output, 1024 * 1024)
                target.chmod((mode & 0o555) | 0o600)
    app = destination / "RepoReach.app"
    require(app.is_dir(), "ZIP has no RepoReach app")
    marker = app / "Contents/Resources/LocalValidation.txt"
    require(marker.is_file() and marker.stat().st_size <= 8192 and marker.read_text().startswith(MARKER), "App lacks its local-validation marker")
    require(not (app / packaging.MODULE_PATH / "Contents/embedded.provisionprofile").exists(), "Input validation module already contains a provisioning profile")
    return app


def identity_certificate(fingerprint, team):
    require(re.fullmatch(r"[0-9A-F]{40}", fingerprint) is not None, "Signing identity must be its full uppercase SHA-1 fingerprint")
    require(re.fullmatch(r"[A-Z0-9]{10}", team) is not None, "Expected developer team must be a ten-character team ID")
    identities = run("security", "find-identity", "-v", "-p", "codesigning").decode()
    matches = re.findall(r'\b' + fingerprint + r'\s+"([^"\n]+)"', identities)
    require(len(matches) == 1 and matches[0].startswith("Developer ID Application:") and matches[0].endswith(f"({team})"), "Expected valid Developer ID identity and team are not available locally")
    encoded = run("security", "find-certificate", "-a", "-c", matches[0], "-p")
    certificates = [base64.b64decode(re.sub(rb"\s", b"", body), validate=True) for body in re.findall(rb"-----BEGIN CERTIFICATE-----\s*(.*?)\s*-----END CERTIFICATE-----", encoded, re.S)]
    matching = [certificate for certificate in certificates if hashlib.sha1(certificate).hexdigest().upper() == fingerprint]
    require(len(matching) == 1, "Public signing certificate does not match the selected local identity")
    require(packaging.certificate_extensions(matching[0]).get(packaging.encoded_oid("1.2.840.113635.100.6.1.13")) == b"\x05\0", "Selected certificate lacks the Developer ID Application marker")
    return matching[0]


def components(app, arch, entitlements):
    module = app / packaging.MODULE_PATH
    finder = app / "Contents/PlugIns/RepoReachFinder.appex"
    for bundle in (app, module, finder):
        info = plistlib.loads((bundle / "Contents/Info.plist").read_bytes())
        executable = info.get("CFBundleExecutable")
        require(isinstance(executable, str) and bool(executable) and pathlib.PurePosixPath(executable).name == executable and "\\" not in executable and executable not in (".", ".."), "Unsafe bundle executable name")
        binary = bundle / "Contents/MacOS" / executable
        require(binary.is_file() and bool(binary.stat().st_mode & 0o111), "Bundle executable is missing or not executable")
    require(info.get("CFBundleIdentifier") == "com.enoughtools.reporeach.finder", "Unexpected Finder extension identifier")
    packaging.check_bundle(app, arch, entitlements)
    paths = [app / "Contents/Helpers" / name for name in ("artifact-fs", "gh")]
    for binary in [*paths, finder / "Contents/MacOS" / executable]:
        require(binary.is_file() and bool(binary.stat().st_mode & 0o111), "Bundled helper or Finder executable is missing or not executable")
        require(run("lipo", "-archs", str(binary)).decode().split() == [arch], "Bundled helper or Finder architecture differs")
    return [*paths, module, finder, app]


def participant_entitlements(revision, team):
    claims = {}
    for name, source in (("app", "native/App/App.entitlements"), ("helper", "native/Helpers/artifact-fs.entitlements")):
        template = plistlib.loads(run("git", "-C", str(ROOT), "show", revision + ":" + source))
        require(template == {packaging.APP_GROUPS: [packaging.GROUP_TEMPLATE]}, "Committed app and engine templates must claim only the Team-prefix app group")
        claims[name] = packaging.resolve_entitlements(template, team)
    return claims


def verify_signature(path, fingerprint, team, private):
    run("codesign", "--verify", "--strict", str(path))
    result = subprocess.run(["codesign", "-d", "--verbose=4", str(path)], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    require(result.returncode == 0, "Cannot inspect signed component")
    details = (result.stdout + result.stderr).decode()
    require("Authority=Developer ID Application:" in details and f"TeamIdentifier={team}\n" in details and "runtime" in details and "Timestamp=" in details, "Signed component lacks the expected Developer ID, team, hardened runtime or timestamp")
    prefix = private / "selected-certificate"
    run("codesign", "-d", f"--extract-certificates={prefix}", str(path))
    require(hashlib.sha1(pathlib.Path(str(prefix) + "0").read_bytes()).hexdigest().upper() == fingerprint, "Signed component used a different identity")


def output_base():
    base = ROOT / "build/fskit-validation/signed"
    path = ROOT
    for part in base.relative_to(ROOT).parts:
        path /= part
        require(not path.is_symlink(), "Local validation output must not pass through symlinks")
        path.mkdir(mode=0o700, exist_ok=True)
        properties = path.lstat()
        require(stat.S_ISDIR(properties.st_mode) and properties.st_uid == os.geteuid() and not properties.st_mode & 0o022, "Local validation output parents must be directories owned and controlled by the current user")
    return base


def sign(args):
    require(sys.platform == "darwin", "Run local signing on the Mac holding the existing identity")
    document = verify_metadata(args)
    utility_source = verify_source(args, document)
    workflow = verify_workflow(args)
    certificate = identity_certificate(args.identity, args.team)
    require(args.profile.is_file() and 0 < args.profile.stat().st_size <= 2 * 1024 * 1024, "Matching FSKit profile is missing or too large")
    base = output_base()
    destination = base / f"{args.arch}-{args.source_revision[:12]}-{args.archive_sha256[:12]}"
    require(not destination.exists(), "Refusing to overwrite a signed validation product")
    with tempfile.TemporaryDirectory(prefix=".signing-", dir=base) as folder:
        private = pathlib.Path(folder)
        archive = private / "verified-input.zip"
        shutil.copyfile(args.archive, archive)
        require(digest(archive) == args.archive_sha256, "Validation archive changed before signing")
        profile = private / "module.provisionprofile"
        profile.write_bytes(args.profile.read_bytes())
        template = plistlib.loads(run("git", "-C", str(ROOT), "show", args.source_revision + ":native/FSKitExtension/FSKit.entitlements"))
        require(template.get(packaging.APP_GROUPS) == [packaging.GROUP_TEMPLATE], "Committed FSKit source must configure its Team-prefix IPC group")
        entitlements = packaging.resolve_entitlements(template, args.team)
        claims = packaging.authorize_profile(packaging.decode_profile(profile), packaging.MODULE_ID, entitlements, certificate=certificate, team=args.team)
        claims_path = private / "module.entitlements"
        claims_path.write_bytes(plistlib.dumps(claims))
        finder_claims = private / "finder.entitlements"
        finder_claims.write_bytes(run("git", "-C", str(ROOT), "show", args.source_revision + ":native/FinderExtension/Finder.entitlements"))
        require(isinstance(plistlib.loads(finder_claims.read_bytes()), dict), "Invalid committed Finder entitlements")
        participant_claims = {}
        for name, value in participant_entitlements(args.source_revision, args.team).items():
            participant_claims[name] = private / (name + ".entitlements")
            participant_claims[name].write_bytes(plistlib.dumps(value))
        app = extract_app(archive, private)
        paths = components(app, args.arch, entitlements)
        app_group = packaging.configure_app_group(app, args.team)
        shutil.copyfile(profile, paths[2] / "Contents/embedded.provisionprofile")
        for index, path in enumerate(paths):
            arguments = ["codesign", "--force", "--timestamp", "--options", "runtime"]
            if index == 0:
                arguments += ["--entitlements", str(participant_claims["helper"])]
            elif index == 2:
                arguments += ["--entitlements", str(claims_path)]
            elif index == 3:
                arguments += ["--entitlements", str(finder_claims)]
            elif index == 4:
                arguments += ["--entitlements", str(participant_claims["app"])]
            run(*arguments, "--sign", args.identity, str(path))
        for path in paths:
            verify_signature(path, args.identity, args.team, private)
        packaging.signed(app, args.arch, entitlements)
        # Publish the directory only after every component and profile passes.
        product = private / "product"
        product.mkdir()
        app.rename(product / app.name)
        signed_app = product / app.name
        files = {str(path.relative_to(signed_app)): digest(path) for path in sorted(signed_app.rglob("*")) if path.is_file()}
        signed_metadata = {
            "purpose": "local-fskit-validation", "distribution": False,
            "source": document["source"], "signingUtilitySource": utility_source,
            "workflow": workflow, "architecture": args.arch, "filesystemBackend": "fskit",
            "inputArtifact": document["artifact"], "signature": "developer-id",
            "signingIdentitySha1": args.identity, "teamIdentifier": args.team,
            "appGroupIdentifier": app_group,
            "embeddedProfileSha256": digest(profile), "notarized": False,
            "extensionActivationAuthorized": False, "mountedValidationPassed": False,
            "signedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "appFileSha256": files,
        }
        (product / "local-signing.json").write_text(json.dumps(signed_metadata, indent=2) + "\n")
        destination.mkdir(mode=0o700)  # Exclusive creation also refuses concurrent output.
        for path in product.iterdir():
            path.rename(destination / path.name)
    print(f"Signed isolated validation app: {destination / 'RepoReach.app'}")
    print("Extension enablement and real mounted validation remain separate steps.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=pathlib.Path, required=True)
    parser.add_argument("--metadata", type=pathlib.Path, required=True)
    parser.add_argument("--archive-sha256", required=True)
    parser.add_argument("--source-revision", required=True)
    parser.add_argument("--source-sha256", required=True)
    parser.add_argument("--workflow-run", type=int, required=True)
    parser.add_argument("--arch", choices=("arm64", "x86_64"), required=True)
    parser.add_argument("--identity", required=True)
    parser.add_argument("--team", required=True)
    parser.add_argument("--profile", type=pathlib.Path, required=True)
    args = parser.parse_args()
    try:
        sign(args)
    except (ValueError, OSError, KeyError, TypeError, UnicodeError, plistlib.InvalidFileException, zipfile.BadZipFile, tarfile.TarError) as failure:
        raise SystemExit(f"Local validation signing failed: {failure}")


if __name__ == "__main__":
    main()
