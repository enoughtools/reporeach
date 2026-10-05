#!/usr/bin/env python3
"""Check FSKit compile output and require its real profile for distribution.

This validates packaging claims, not OS activation. A production release also
needs a successful signed, mounted FSKit test on macOS 26.
"""
import argparse
import base64
import datetime
import pathlib
import plistlib
import re
import subprocess
import tempfile

MODULE_ID = "com.enoughtools.reporeach.fskit"
MODULE_PATH = "Contents/Extensions/RepoReachFSKit.appex"
FSMODULE = "com.apple.developer.fskit.fsmodule"
SANDBOX = "com.apple.security.app-sandbox"
IDENTIFIERS = ("com.apple.application-identifier", "application-identifier")


def require(condition, message):
    if not condition:
        raise ValueError(message)


def run(*args):
    process = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if process.returncode:
        raise ValueError(f"{args[0]} failed while validating FSKit packaging")
    return process.stdout, process.stderr


def der_parts(encoded):
    result, position = [], 0
    while position < len(encoded):
        require(position + 2 <= len(encoded), "Truncated certificate DER")
        tag, length = encoded[position], encoded[position + 1]
        position += 2
        require(tag & 31 != 31, "Unsupported certificate DER tag")
        if length & 128:
            width = length & 127
            require(0 < width <= 4 and position + width <= len(encoded), "Invalid certificate DER length")
            length = int.from_bytes(encoded[position:position + width], "big")
            position += width
        require(position + length <= len(encoded), "Certificate DER exceeds input")
        result.append((tag, encoded[position:position + length]))
        position += length
    return result


def encoded_oid(text):
    components = [int(value) for value in text.split(".")]
    result = bytearray()
    for value in [components[0] * 40 + components[1]] + components[2:]:
        group = [value & 127]
        value >>= 7
        while value:
            group.insert(0, 128 | (value & 127))
            value >>= 7
        result.extend(group)
    return bytes(result)


def certificate_extensions(encoded):
    certificate = der_parts(encoded)
    require(len(certificate) == 1 and certificate[0][0] == 48, "Certificate is not a DER sequence")
    fields = der_parts(certificate[0][1])
    require(len(fields) == 3 and fields[0][0] == 48, "Invalid X509 certificate structure")
    groups = [value for tag, value in der_parts(fields[0][1]) if tag == 163]
    require(len(groups) == 1, "Certificate has no unique extension group")
    group = der_parts(groups[0])
    require(len(group) == 1 and group[0][0] == 48, "Invalid X509 extension group")
    extensions = {}
    for tag, value in der_parts(group[0][1]):
        require(tag == 48, "Invalid X509 extension sequence")
        parts = der_parts(value)
        require(len(parts) in (2, 3) and parts[0][0] == 6 and parts[-1][0] == 4, "Invalid X509 extension value")
        require(len(parts) == 2 or parts[1][0] == 1, "Invalid X509 critical flag")
        require(parts[0][1] not in extensions, "Duplicate X509 extension")
        extensions[parts[0][1]] = parts[-1][1]
    return extensions


def verify_profile_signer(leaf, issuer):
    # Apple's published OSX profile policy uses these marker extensions; the
    # values must be canonical ASN.1 NULL, not arbitrary matching text/bytes.
    # https://github.com/apple-oss-distributions/Security/blob/db15acbe6a7f257a859ad9a3bb86097bfe0679d9/OSX/sec/Security/SecPolicy.c#L3286
    leaf_extensions, issuer_extensions = certificate_extensions(leaf), certificate_extensions(issuer)
    require(leaf_extensions.get(encoded_oid("1.2.840.113635.100.4.11")) == b"\x05\x00", "CMS signer lacks the macOS provisioning authority marker")
    require(issuer_extensions.get(encoded_oid("1.2.840.113635.100.6.2.1")) == b"\x05\x00", "CMS issuer lacks the WWDR authority marker")
    usage = der_parts(leaf_extensions.get(encoded_oid("2.5.29.15"), b""))
    require(len(usage) == 1 and usage[0][0] == 3 and len(usage[0][1]) >= 2 and usage[0][1][0] <= 7 and usage[0][1][1] & 128, "Profile authority lacks digital-signature key usage")


def decode_profile(path):
    # CMS decoding alone accepts unsigned containers. Authenticate the signer
    # through public Security APIs, only Apple's immutable system root anchors,
    # current certificate validity and OCSP before using any payload claims.
    roots, _ = run("security", "find-certificate", "-a", "-c", "Apple Root CA", "-p", "/System/Library/Keychains/SystemRootCertificates.keychain")
    certificates = re.findall(rb"-----BEGIN CERTIFICATE-----\s*(.*?)\s*-----END CERTIFICATE-----", roots, re.S)
    require(bool(certificates), "System Apple root anchors are unavailable")
    with tempfile.TemporaryDirectory(prefix="reporeach-profile-auth-") as folder:
        private = pathlib.Path(folder)
        root_paths = []
        for index, pem in enumerate(certificates):
            root = private / f"apple-root-{index}.der"
            root.write_bytes(base64.b64decode(re.sub(rb"\s", b"", pem), validate=True))
            root_paths.append(str(root))
        verifier = private / "verify-apple-profile"
        run("xcrun", "clang", "-std=c11", "-O2", "-framework", "Security", "-framework", "CoreFoundation", str(pathlib.Path(__file__).with_name("verify-apple-profile.c")), "-o", str(verifier))
        run(str(verifier), str(path), str(private), *root_paths)
        verify_profile_signer((private / "signer.der").read_bytes(), (private / "issuer.der").read_bytes())
        profile = plistlib.loads((private / "payload.plist").read_bytes())
    require(isinstance(profile, dict), "Invalid provisioning profile document")
    return profile


def utc(value):
    require(isinstance(value, datetime.datetime), "Profile has no validity date")
    return value.replace(tzinfo=datetime.timezone.utc) if value.tzinfo is None else value.astimezone(datetime.timezone.utc)


def authorize_profile(profile, bundle_id, requested, now=None, certificate=None, team=None, require_bound=False):
    now = now or datetime.datetime.now(datetime.timezone.utc)
    require("OSX" in profile.get("Platform", []), "FSKit needs a macOS provisioning profile")
    require(utc(profile.get("CreationDate")) <= now < utc(profile.get("ExpirationDate")), "Provisioning profile is not currently valid")
    require(profile.get("ProvisionsAllDevices") is True, "FSKit distribution needs a Developer ID profile")
    require(not profile.get("ProvisionedDevices"), "A development device profile cannot authorize distribution")
    allowed = profile.get("Entitlements", {})
    require(isinstance(allowed, dict), "Profile has no entitlement authorization")
    require(allowed.get(FSMODULE) is True, "Profile does not authorize the FSKit Module capability")
    for debug_key in ("get-task-allow", "com.apple.security.get-task-allow"):
        require(not allowed.get(debug_key, False) and not requested.get(debug_key, False), "Debug task access is forbidden in distribution")
    teams = profile.get("TeamIdentifier", [])
    require(isinstance(teams, list) and len(teams) == 1 and bool(teams[0]), "Profile must identify one developer team")
    profile_team = teams[0]
    require(team is None or team == profile_team, "Signing identity and profile developer teams differ")
    require(allowed.get("com.apple.developer.team-identifier") == profile_team, "Profile team entitlement does not match its team")
    prefixes = profile.get("ApplicationIdentifierPrefix", [])
    require(isinstance(prefixes, list) and bool(prefixes) and all(isinstance(prefix, str) for prefix in prefixes), "Profile has no valid application identifier prefix")
    application_id_key = next((key for key in IDENTIFIERS if key in allowed), None)
    require(application_id_key is not None, "Profile has no application identifier")
    application_id = allowed[application_id_key]
    require(isinstance(application_id, str) and "*" not in application_id, "FSKit needs an explicit module App ID")
    require(any(application_id == f"{prefix}.{bundle_id}" for prefix in prefixes), "Provisioning profile belongs to a different extension")
    require(requested.get(FSMODULE) is True and requested.get(SANDBOX) is True, "FSKit module must claim its capability and sandbox")
    for key in IDENTIFIERS:
        if key in requested:
            require(key == application_id_key and requested[key] == application_id, "Signed application identifier differs from its profile")
    if "com.apple.developer.team-identifier" in requested:
        require(requested["com.apple.developer.team-identifier"] == profile_team, "Signed developer team entitlement differs from its profile")
    # Restricted developer claims need profile authorization. Sandbox and
    # ordinary security entitlements do not have to appear in the profile.
    for key, value in requested.items():
        if key.startswith("com.apple.developer."):
            require(allowed.get(key) == value, f"Profile does not authorize requested capability {key}")
    for group in requested.get("com.apple.security.application-groups", []):
        if group.startswith("group."):
            require(group in allowed.get("com.apple.security.application-groups", []), "Profile does not authorize the modern app group")
    certificates = profile.get("DeveloperCertificates", [])
    require(bool(certificates) and all(isinstance(cert, bytes) for cert in certificates), "Profile has no authorized signing certificates")
    if certificate is not None:
        require(certificate in certificates, "Actual signing certificate is not authorized by this profile")
    result = dict(requested)
    result[application_id_key] = application_id
    result["com.apple.developer.team-identifier"] = profile_team
    if require_bound:
        require(all(requested.get(key) == value for key, value in result.items()), "Final FSKit signature must contain its profile-bound application identifier and team")
    return result


def check_bundle(app, arch, expected_entitlements):
    module = app / MODULE_PATH
    require(module.is_dir(), "FSKit module is missing from Contents/Extensions")
    require(not (app / "Contents/PlugIns/RepoReachFSKit.appex").exists(), "FSKit must use the ExtensionKit embedding directory")
    app_info = plistlib.loads((app / "Contents/Info.plist").read_bytes())
    info = plistlib.loads((module / "Contents/Info.plist").read_bytes())
    require(app_info.get("CFBundleIdentifier") == "com.enoughtools.reporeach", "Unexpected containing app identifier")
    require(info.get("CFBundleIdentifier") == MODULE_ID, "Unexpected FSKit module identifier")
    require(info.get("EXAppExtensionAttributes", {}).get("EXExtensionPointIdentifier") == "com.apple.fskit.fsmodule", "FSKit extension point is not configured")
    version = tuple(int(component) for component in str(info.get("LSMinimumSystemVersion", "0")).split("."))
    require(version >= (26, 0), "FSKit module must require macOS 26")
    require(expected_entitlements.get(FSMODULE) is True and expected_entitlements.get(SANDBOX) is True, "FSKit capability and sandbox must be configured")
    for bundle, properties in ((app, app_info), (module, info)):
        binary = bundle / "Contents/MacOS" / properties["CFBundleExecutable"]
        slices, _ = run("lipo", "-archs", str(binary))
        require(slices.decode().split() == [arch], "Compiled bundle architecture differs from requested architecture")
    return module


def signed(app, arch, expected_entitlements):
    module = check_bundle(app, arch, expected_entitlements)
    embedded = module / "Contents/embedded.provisionprofile"
    require(embedded.is_file(), "Signed FSKit module lacks its own embedded provisioning profile")
    run("codesign", "--verify", "--strict", str(module))
    details_out, details_err = run("codesign", "-d", "--verbose=4", str(module))
    details = (details_out + details_err).decode()
    require("Authority=Developer ID Application:" in details, "FSKit distribution requires a Developer ID identity")
    require("runtime" in details and "Timestamp=" in details, "FSKit distribution requires hardened runtime and a signing timestamp")
    team = next((line.split("=", 1)[1] for line in details.splitlines() if line.startswith("TeamIdentifier=")), None)
    claims, _ = run("codesign", "-d", "--entitlements", "-", str(module))
    actual = plistlib.loads(claims)
    require(all(actual.get(key) == value for key, value in expected_entitlements.items()), "Actual FSKit signature omits configured entitlement claims")
    with tempfile.TemporaryDirectory(prefix="reporeach-fskit-cert-") as private:
        certificate_path = pathlib.Path(private) / "certificate"
        run("codesign", "-d", "--extract-certificates", str(certificate_path), str(module))
        certificate = pathlib.Path(str(certificate_path) + "0").read_bytes()
    authorize_profile(decode_profile(embedded), MODULE_ID, actual, certificate=certificate, team=team, require_bound=True)
    run("codesign", "--verify", "--strict", str(app))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for name in ("compile", "signed"):
        command = commands.add_parser(name)
        command.add_argument("--app", type=pathlib.Path, required=True)
        command.add_argument("--arch", choices=["arm64", "x86_64"], required=True)
        command.add_argument("--entitlements", type=pathlib.Path, required=True)
    prepare = commands.add_parser("prepare")
    prepare.add_argument("--profile", type=pathlib.Path, required=True)
    prepare.add_argument("--bundle-id", required=True)
    prepare.add_argument("--entitlements", type=pathlib.Path, required=True)
    prepare.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    try:
        entitlements = plistlib.loads(args.entitlements.read_bytes())
        if args.command == "prepare":
            claims = authorize_profile(decode_profile(args.profile), args.bundle_id, entitlements)
            args.output.write_bytes(plistlib.dumps(claims))
        elif args.command == "compile":
            check_bundle(args.app, args.arch, entitlements)
        else:
            signed(args.app, args.arch, entitlements)
    except (ValueError, OSError, plistlib.InvalidFileException, KeyError) as failure:
        raise SystemExit(f"FSKit packaging validation failed: {failure}")
    print(f"FSKit {args.command} packaging checks passed")


if __name__ == "__main__":
    main()
