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
APP_GROUPS = "com.apple.security.application-groups"
GROUP_TEMPLATE = "$(TeamIdentifierPrefix)rr"
GROUP_INFO = "RepoReachAppGroupIdentifier"
UNRESOLVED_GROUPS = (".rr", "$(DEVELOPMENT_TEAM).rr")


def app_group_identifier(team):
    require(isinstance(team, str) and re.fullmatch(r"[A-Z0-9]{10}", team), "Developer team must be a ten-character team ID")
    return team + ".rr"


def resolve_entitlements(requested, team):
    require(isinstance(requested, dict), "Entitlements must be a dictionary")
    result = dict(requested)
    if APP_GROUPS in result:
        groups = result[APP_GROUPS]
        require(isinstance(groups, list) and bool(groups) and all(isinstance(group, str) for group in groups), "App groups must be a nonempty array of identifiers")
        group = app_group_identifier(team)
        result[APP_GROUPS] = [group if value == GROUP_TEMPLATE else value for value in groups]
    return result


def validate_app_groups(requested, team, allowed):
    if APP_GROUPS not in requested:
        return
    groups = requested[APP_GROUPS]
    require(isinstance(groups, list) and len(groups) == 1 and isinstance(groups[0], str), "RepoReach must claim exactly one app group identifier")
    group = groups[0]
    if group.startswith("group."):
        require(re.fullmatch(r"group\.[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*", group), "Invalid modern app group identifier")
        authorized = allowed.get(APP_GROUPS, [])
        require(isinstance(authorized, list) and all(isinstance(value, str) for value in authorized) and group in authorized, "Profile does not authorize the modern app group")
    else:
        # macOS Team-prefix groups are code-signature claims; they are not
        # restricted capabilities requiring a profile or portal registration.
        # The prefix comes from the signing team, never an older App ID prefix.
        require(group == app_group_identifier(team), "RepoReach app group must match the actual signing team and rr identifier")


def check_group_metadata(app, team=None, allow_unresolved=False):
    values = []
    for bundle in (app, app / MODULE_PATH):
        info = plistlib.loads((bundle / "Contents/Info.plist").read_bytes())
        values.append(info.get(GROUP_INFO))
    require(all(isinstance(value, str) for value in values) and values[0] == values[1], "App and FSKit module must declare the same app group")
    group = values[0]
    if allow_unresolved and group in UNRESOLVED_GROUPS:
        return group
    require(re.fullmatch(r"[A-Z0-9]{10}\.rr", group) is not None, "App group metadata is unresolved or invalid; authorized local signing is required")
    require(team is None or group == app_group_identifier(team), "Runtime app group differs from the actual signing team")
    return group


def configure_app_group(app, team):
    group = app_group_identifier(team)
    prior = check_group_metadata(app, allow_unresolved=True)
    require(prior in (*UNRESOLVED_GROUPS, group), "Compiled app group belongs to another developer team")
    for bundle in (app, app / MODULE_PATH):
        path = bundle / "Contents/Info.plist"
        info = plistlib.loads(path.read_bytes())
        info[GROUP_INFO] = group
        path.write_bytes(plistlib.dumps(info))
    return group


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
    require(isinstance(requested, dict), "Requested entitlements must be a dictionary")
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
    require(isinstance(teams, list) and len(teams) == 1, "Profile must identify one developer team")
    profile_team = teams[0]
    app_group_identifier(profile_team)
    require(team is None or team == profile_team, "Signing identity and profile developer teams differ")
    if require_bound:
        validate_app_groups(requested, profile_team, allowed)
    requested = resolve_entitlements(requested, profile_team)
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
    validate_app_groups(requested, profile_team, allowed)
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


def check_bundle(app, arch, expected_entitlements, allow_unresolved=True):
    module = app / MODULE_PATH
    require(module.is_dir(), "FSKit module is missing from Contents/Extensions")
    require(not (app / "Contents/PlugIns/RepoReachFSKit.appex").exists(), "FSKit must use the ExtensionKit embedding directory")
    app_info = plistlib.loads((app / "Contents/Info.plist").read_bytes())
    info = plistlib.loads((module / "Contents/Info.plist").read_bytes())
    require(app_info.get("CFBundleIdentifier") == "com.enoughtools.reporeach", "Unexpected containing app identifier")
    require(info.get("CFBundleIdentifier") == MODULE_ID, "Unexpected FSKit module identifier")
    attributes = info.get("EXAppExtensionAttributes", {})
    require(isinstance(attributes, dict) and attributes.get("EXExtensionPointIdentifier") == "com.apple.fskit.fsmodule", "FSKit extension point is not configured")
    # mount's FSKit dispatcher advertises activation from this dictionary. Its
    # common -o syntax propagates resource read/write mount flags; RepoReach
    # implements no additional activation, checking or formatting switches.
    require(attributes.get("FSActivateOptionSyntax") == {"shortOptions": "o:"}, "FSKit mount activation requires the supported common -o option syntax")
    require("FSCheckOptionSyntax" not in attributes and "FSFormatOptionSyntax" not in attributes, "FSKit module must not advertise unsupported checking or formatting operations")
    version = tuple(int(component) for component in str(info.get("LSMinimumSystemVersion", "0")).split("."))
    require(version >= (26, 0), "FSKit module must require macOS 26")
    require(expected_entitlements.get(FSMODULE) is True and expected_entitlements.get(SANDBOX) is True, "FSKit capability and sandbox must be configured")
    require(set(expected_entitlements) == {FSMODULE, SANDBOX, APP_GROUPS}, "FSKit source must configure only its capability, sandbox and IPC group")
    require(expected_entitlements.get(APP_GROUPS) == [GROUP_TEMPLATE] or (
        isinstance(expected_entitlements.get(APP_GROUPS), list)
        and len(expected_entitlements[APP_GROUPS]) == 1
        and isinstance(expected_entitlements[APP_GROUPS][0], str)
        and re.fullmatch(r"[A-Z0-9]{10}\.rr", expected_entitlements[APP_GROUPS][0]) is not None
    ), "FSKit must configure its single Team-prefix app group")
    check_group_metadata(app, allow_unresolved=allow_unresolved)
    for bundle, properties in ((app, app_info), (module, info)):
        binary = bundle / "Contents/MacOS" / properties["CFBundleExecutable"]
        slices, _ = run("lipo", "-archs", str(binary))
        require(slices.decode().split() == [arch], "Compiled bundle architecture differs from requested architecture")
    return module


def signature_entitlements(path, allow_empty=False):
    # Request a property list explicitly; default display may be human-readable.
    claims, _ = run("codesign", "-d", "--entitlements", "-", "--xml", str(path))
    if allow_empty and not claims.strip():
        return {}
    actual = plistlib.loads(claims)
    require(isinstance(actual, dict), "Actual signature entitlements must be a dictionary")
    return actual


def signing_identity(path):
    run("codesign", "--verify", "--strict", str(path))
    details_out, details_err = run("codesign", "-d", "--verbose=4", str(path))
    details = (details_out + details_err).decode()
    require("Authority=Developer ID Application:" in details, "FSKit distribution requires a Developer ID Application identity")
    require("runtime" in details and "Timestamp=" in details, "FSKit distribution requires hardened runtime and a signing timestamp")
    teams = [line.split("=", 1)[1] for line in details.splitlines() if line.startswith("TeamIdentifier=")]
    require(len(teams) == 1, "Signed component must identify one developer team")
    team = teams[0]
    app_group_identifier(team)
    with tempfile.TemporaryDirectory(prefix="reporeach-fskit-cert-") as private:
        certificate_path = pathlib.Path(private) / "certificate"
        run("codesign", "-d", f"--extract-certificates={certificate_path}", str(path))
        certificate = pathlib.Path(str(certificate_path) + "0").read_bytes()
    require(certificate_extensions(certificate).get(encoded_oid("1.2.840.113635.100.6.1.13")) == b"\x05\0", "Actual signing certificate lacks the Developer ID Application marker")
    return team, certificate


def signed(app, arch, expected_entitlements):
    module = check_bundle(app, arch, expected_entitlements, allow_unresolved=False)
    embedded = module / "Contents/embedded.provisionprofile"
    require(embedded.is_file(), "Signed FSKit module lacks its own embedded provisioning profile")
    team, certificate = signing_identity(module)
    group = check_group_metadata(app, team=team)
    actual = signature_entitlements(module)
    expected = resolve_entitlements(expected_entitlements, team)
    require(all(actual.get(key) == value for key, value in expected.items()), "Actual FSKit signature omits configured entitlement claims")
    require(actual.get(APP_GROUPS) == [group], "FSKit signature app group differs from runtime metadata")
    require(set(actual) <= set(expected) | set(IDENTIFIERS) | {"com.apple.developer.team-identifier"}, "Actual FSKit signature contains unexpected entitlement claims")
    authorize_profile(decode_profile(embedded), MODULE_ID, actual, certificate=certificate, team=team, require_bound=True)
    participants = (app / "Contents/Helpers/artifact-fs", app)
    for component in participants:
        component_team, component_certificate = signing_identity(component)
        require((component_team, component_certificate) == (team, certificate), "IPC participants must use the same actual signing team and certificate")
        require(signature_entitlements(component) == {APP_GROUPS: [group]}, "App and engine must claim only their matching Team-prefix app group")
    for component in (app / "Contents/Helpers/gh", app / "Contents/PlugIns/RepoReachFinder.appex"):
        component_team, component_certificate = signing_identity(component)
        require((component_team, component_certificate) == (team, certificate), "Bundled component used a different signing team or certificate")
        claims = signature_entitlements(component, allow_empty=True)
        require(APP_GROUPS not in claims, "GitHub CLI and Finder extension must not claim the filesystem IPC group")
        require(not any(claims.get(key, False) for key in ("get-task-allow", "com.apple.security.get-task-allow")), "Debug task access is forbidden in distribution")


def prepare_signing(args):
    team, certificate = signing_identity(args.signing_component)
    requested = plistlib.loads(args.entitlements.read_bytes())
    require(isinstance(requested, dict) and set(requested) == {FSMODULE, SANDBOX, APP_GROUPS}, "FSKit source must configure only its capability, sandbox and IPC group")
    module_claims = authorize_profile(decode_profile(args.profile), args.bundle_id, requested, certificate=certificate, team=team)
    require(module_claims.get(APP_GROUPS) == [app_group_identifier(team)], "FSKit must claim its matching Team-prefix app group")
    participant_claims = {}
    for name, source in (("app", args.app_entitlements), ("helper", args.helper_entitlements)):
        claims = resolve_entitlements(plistlib.loads(source.read_bytes()), team)
        require(claims == {APP_GROUPS: [app_group_identifier(team)]}, "App and engine entitlement templates must claim only the matching app group")
        participant_claims[name] = claims
    configure_app_group(args.app, team)
    args.output_directory.mkdir(parents=True, exist_ok=True)
    for name, claims in {"module": module_claims, **participant_claims}.items():
        (args.output_directory / (name + ".entitlements")).write_bytes(plistlib.dumps(claims))


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
    prepare.add_argument("--app", type=pathlib.Path, required=True)
    prepare.add_argument("--signing-component", type=pathlib.Path, required=True)
    prepare.add_argument("--app-entitlements", type=pathlib.Path, required=True)
    prepare.add_argument("--helper-entitlements", type=pathlib.Path, required=True)
    prepare.add_argument("--output-directory", type=pathlib.Path, required=True)
    args = parser.parse_args()
    try:
        entitlements = plistlib.loads(args.entitlements.read_bytes())
        if args.command == "prepare":
            prepare_signing(args)
        elif args.command == "compile":
            check_bundle(args.app, args.arch, entitlements)
        else:
            signed(args.app, args.arch, entitlements)
    except (ValueError, OSError, plistlib.InvalidFileException, KeyError, TypeError) as failure:
        raise SystemExit(f"FSKit packaging validation failed: {failure}")
    print(f"FSKit {args.command} packaging checks passed")


if __name__ == "__main__":
    main()
