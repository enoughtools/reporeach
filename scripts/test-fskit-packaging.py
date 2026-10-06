#!/usr/bin/env python3
"""Regression checks for distribution authorization; fixtures are not profiles."""
import copy
import datetime
import importlib.util
import json
import pathlib
import plistlib
import subprocess
import sys
import tempfile
import unittest
import zipfile
from types import SimpleNamespace
from unittest import mock

source = pathlib.Path(__file__).with_name("validate-fskit-bundle.py")
spec = importlib.util.spec_from_file_location("fskit_packaging", source)
packaging = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packaging)
validation_source = pathlib.Path(__file__).with_name("validation-artifact.py")
validation_spec = importlib.util.spec_from_file_location("local_validation", validation_source)
local_validation = importlib.util.module_from_spec(validation_spec)
validation_spec.loader.exec_module(local_validation)


class ProfileAuthorizationTests(unittest.TestCase):
    def setUp(self):
        self.now = datetime.datetime(2026, 10, 4, tzinfo=datetime.timezone.utc)
        self.team = "FIXTURE123"
        self.module = packaging.MODULE_ID
        self.certificate = b"fixture certificate bytes, never used for signing"
        self.profile = {
            "Platform": ["OSX"],
            "CreationDate": self.now - datetime.timedelta(days=1),
            "ExpirationDate": self.now + datetime.timedelta(days=1),
            "ProvisionsAllDevices": True,
            "TeamIdentifier": [self.team],
            "ApplicationIdentifierPrefix": [self.team],
            "DeveloperCertificates": [self.certificate],
            "Entitlements": {
                "com.apple.application-identifier": f"{self.team}.{self.module}",
                "com.apple.developer.team-identifier": self.team,
                packaging.FSMODULE: True,
            },
        }
        self.requested = {
            packaging.FSMODULE: True,
            packaging.SANDBOX: True,
            "com.apple.security.network.client": True,
        }

    def authorize(self, profile=None, requested=None, **extra):
        return packaging.authorize_profile(
            self.profile if profile is None else profile,
            self.module,
            self.requested if requested is None else requested,
            now=self.now,
            certificate=extra.get("certificate", self.certificate),
            team=extra.get("team", self.team),
            require_bound=extra.get("require_bound", False),
        )

    def test_distribution_claims_bind_the_actual_module_and_team(self):
        claims = self.authorize()
        self.assertEqual(claims["com.apple.application-identifier"], f"{self.team}.{self.module}")
        self.assertEqual(claims["com.apple.developer.team-identifier"], self.team)
        # Unrestricted sandbox/network claims need not be in the profile.
        self.assertTrue(claims[packaging.SANDBOX])
        self.assertNotIn("com.apple.application-identifier", self.requested)

    def test_rejects_wrong_distribution_scope_and_dates(self):
        changes = (
            {"Platform": ["iOS"]},
            {"CreationDate": self.now + datetime.timedelta(seconds=1)},
            {"ExpirationDate": self.now},
            {"ProvisionsAllDevices": False},
            {"ProvisionedDevices": ["fixture-device"]},
            {"TeamIdentifier": [self.team, "OTHERTEAM"]},
            {"DeveloperCertificates": []},
        )
        for change in changes:
            with self.subTest(change=list(change)):
                profile = copy.deepcopy(self.profile)
                profile.update(change)
                with self.assertRaises(ValueError):
                    self.authorize(profile)

    def test_cannot_reuse_host_finder_or_wildcard_profiles(self):
        for suffix in ("com.enoughtools.reporeach", "com.enoughtools.reporeach.finder", "*"):
            with self.subTest(identifier=suffix):
                profile = copy.deepcopy(self.profile)
                profile["Entitlements"]["com.apple.application-identifier"] = f"{self.team}.{suffix}"
                with self.assertRaises(ValueError):
                    self.authorize(profile)

    def test_requires_capability_and_no_debug_access(self):
        for change in ({packaging.FSMODULE: False}, {"get-task-allow": True}, {"com.apple.security.get-task-allow": True}, {"com.apple.developer.team-identifier": "OTHERTEAM"}):
            with self.subTest(change=list(change)):
                profile = copy.deepcopy(self.profile)
                profile["Entitlements"].update(change)
                with self.assertRaises(ValueError):
                    self.authorize(profile)
        for change in ({packaging.SANDBOX: False}, {"get-task-allow": True}, {"com.apple.security.get-task-allow": True}, {"com.apple.developer.unapproved-capability": True}):
            with self.subTest(claims=list(change)):
                with self.assertRaises(ValueError):
                    self.authorize(requested=dict(self.requested, **change))

    def test_rejects_actual_certificate_or_team_mismatch(self):
        with self.assertRaises(ValueError):
            self.authorize(certificate=b"another certificate")
        with self.assertRaises(ValueError):
            self.authorize(team="OTHERTEAM")

    def test_modern_app_group_requires_profile_authorization(self):
        requested = dict(self.requested, **{"com.apple.security.application-groups": ["group.com.enoughtools.reporeach"]})
        with self.assertRaises(ValueError):
            self.authorize(requested=requested)
        profile = copy.deepcopy(self.profile)
        profile["Entitlements"]["com.apple.security.application-groups"] = requested["com.apple.security.application-groups"]
        self.authorize(profile, requested)

    def test_team_prefix_group_uses_signing_team_instead_of_app_id_prefix(self):
        profile = copy.deepcopy(self.profile)
        profile["ApplicationIdentifierPrefix"] = ["OLDPREFIX1"]
        profile["Entitlements"]["com.apple.application-identifier"] = "OLDPREFIX1." + self.module
        requested = dict(self.requested, **{packaging.APP_GROUPS: [packaging.GROUP_TEMPLATE]})
        claims = self.authorize(profile, requested)
        self.assertEqual(claims[packaging.APP_GROUPS], [self.team + ".rr"])
        self.assertNotIn(packaging.APP_GROUPS, profile["Entitlements"])

    def test_team_prefix_group_rejects_malformed_or_foreign_claims(self):
        groups = (None, True, self.team + ".rr", [], [None], [True], [123],
                  [self.team + ".rr", self.team + ".rr"], [self.team + ".rr", "group.example"],
                  ["OTHERTEAM1.rr"], [self.team + ".other"], ["rr"], [".rr"],
                  [self.team + ".*"], [self.team + ".rr/path"], [self.team + ".rr\0"],
                  [self.team + ".r\u0440"])
        for value in groups:
            with self.subTest(groups=value), self.assertRaises(ValueError):
                self.authorize(requested=dict(self.requested, **{packaging.APP_GROUPS: value}))

    def test_final_signature_cannot_keep_unresolved_group_template(self):
        requested = dict(self.requested, **{packaging.APP_GROUPS: [packaging.GROUP_TEMPLATE]})
        claims = self.authorize(requested=requested)
        self.authorize(requested=claims, require_bound=True)
        claims[packaging.APP_GROUPS] = [packaging.GROUP_TEMPLATE]
        with self.assertRaisesRegex(ValueError, "actual signing team"):
            self.authorize(requested=claims, require_bound=True)

    def test_malformed_modern_group_or_profile_authorization_is_rejected(self):
        for group in ("group.", "group.*", "group.example/path", "group..example", "group.\u0435xample"):
            requested = dict(self.requested, **{packaging.APP_GROUPS: [group]})
            profile = copy.deepcopy(self.profile)
            profile["Entitlements"][packaging.APP_GROUPS] = [group]
            with self.subTest(group=group), self.assertRaises(ValueError):
                self.authorize(profile, requested)
        requested = dict(self.requested, **{packaging.APP_GROUPS: ["group.example"]})
        for authorization in ("group.example", ["group.*"], [None], True):
            profile = copy.deepcopy(self.profile)
            profile["Entitlements"][packaging.APP_GROUPS] = authorization
            with self.subTest(authorization=authorization), self.assertRaises(ValueError):
                self.authorize(profile, requested)

    def test_invalid_profile_team_cannot_resolve_a_group(self):
        for team in (None, True, "", "SHORT", "TOO_LONG123", "fixture123", "FIXTURE12\0"):
            profile = copy.deepcopy(self.profile)
            profile["TeamIdentifier"] = [team]
            with self.subTest(team=team), self.assertRaises(ValueError):
                self.authorize(profile)

    def test_final_signature_application_identifier_must_match(self):
        requested = dict(self.requested, **{"com.apple.application-identifier": f"{self.team}.com.enoughtools.reporeach.finder"})
        with self.assertRaises(ValueError):
            self.authorize(requested=requested)

    def test_final_signature_cannot_omit_profile_bound_claims(self):
        claims = self.authorize()
        self.authorize(requested=claims, require_bound=True)
        for key in ("com.apple.application-identifier", "com.apple.developer.team-identifier"):
            with self.subTest(omitted=key):
                partial = dict(claims)
                partial.pop(key)
                with self.assertRaises(ValueError):
                    self.authorize(requested=partial, require_bound=True)


def der(tag, value):
    length = bytes([len(value)]) if len(value) < 128 else bytes([130]) + len(value).to_bytes(2, "big")
    return bytes([tag]) + length + value


def certificate_shape(extensions):
    # These parser fixtures are not valid certificates or signing material.
    records = b"".join(der(48, der(6, packaging.encoded_oid(oid)) + der(4, value)) for oid, value in extensions)
    tbs = der(48, der(163, der(48, records)))
    return der(48, tbs + der(48, b"") + der(3, b"\0"))


class CertificateMarkerTests(unittest.TestCase):
    def test_requires_canonical_profile_and_wwdr_extensions(self):
        issuer = certificate_shape([("1.2.840.113635.100.6.2.1", b"\x05\0")])
        usage = ("2.5.29.15", der(3, b"\x07\x80"))
        leaf = certificate_shape([("1.2.840.113635.100.4.11", b"\x05\0"), usage])
        packaging.verify_profile_signer(leaf, issuer)
        for value in (b"", b"\x05\x01\0", b"\x01\x01\0"):
            with self.subTest(marker=value):
                malformed = certificate_shape([("1.2.840.113635.100.4.11", value), usage])
                with self.assertRaises(ValueError):
                    packaging.verify_profile_signer(malformed, issuer)

    def test_normal_developer_id_or_wrong_issuer_is_not_profile_authority(self):
        usage = ("2.5.29.15", der(3, b"\x07\x80"))
        leaf = certificate_shape([("1.2.840.113635.100.4.11", b"\x05\0"), usage])
        issuer = certificate_shape([("1.2.840.113635.100.6.2.1", b"\x05\0")])
        ordinary = certificate_shape([("1.2.840.113635.100.6.1.13", b"\x05\0"), usage])
        with self.assertRaises(ValueError):
            packaging.verify_profile_signer(ordinary, issuer)
        with self.assertRaises(ValueError):
            packaging.verify_profile_signer(leaf, ordinary)

    def test_rejects_non_signature_key_usage_and_truncated_der(self):
        issuer = certificate_shape([("1.2.840.113635.100.6.2.1", b"\x05\0")])
        leaf = certificate_shape([("1.2.840.113635.100.4.11", b"\x05\0"), ("2.5.29.15", der(3, b"\x05\x20"))])
        with self.assertRaises(ValueError):
            packaging.verify_profile_signer(leaf, issuer)
        with self.assertRaises(ValueError):
            packaging.certificate_extensions(leaf[:-1])


@unittest.skipUnless(sys.platform == "darwin", "Uses public macOS Security APIs")
class CMSAuthenticationTests(unittest.TestCase):
    def test_unsigned_cms_data_cannot_authorize_profile_claims(self):
        with tempfile.TemporaryDirectory(prefix="reporeach-unsigned-cms-test-") as folder:
            path = pathlib.Path(folder) / "unsigned.cms"
            content = plistlib.dumps({"Entitlements": {packaging.FSMODULE: True}})
            path.write_bytes(der(48, der(6, packaging.encoded_oid("1.2.840.113549.1.7.1")) + der(160, der(4, content))))
            with self.assertRaises(ValueError):
                packaging.decode_profile(path)


class SignedEntitlementTests(unittest.TestCase):
    def test_extracts_typed_claims_with_explicit_xml_output(self):
        claims = {packaging.FSMODULE: True, "fixture.identifier": "FIXTURE123.example", "fixture.groups": ["fixture-group"], "fixture.data": b"binary\0\xff"}
        for format in (plistlib.FMT_XML, plistlib.FMT_BINARY):
            with self.subTest(format=format), mock.patch.object(packaging, "run", return_value=(plistlib.dumps(claims, fmt=format), b"display diagnostics")) as run:
                self.assertEqual(packaging.signature_entitlements(pathlib.Path("fixture.appex")), claims)
                run.assert_called_once_with("codesign", "-d", "--entitlements", "-", "--xml", "fixture.appex")

    def test_missing_or_abstract_claims_fail_without_format_fallback(self):
        for contents in (b"", b"[Dict]\n\t[Key] com.apple.developer.fskit.fsmodule\n\t[Value] true\n", b"not a plist"):
            with self.subTest(contents=contents), mock.patch.object(packaging, "run", return_value=(contents, b"")) as run, self.assertRaises(ValueError):
                packaging.signature_entitlements(pathlib.Path("fixture.appex"))
            self.assertEqual(run.call_count, 1)

    def test_non_dictionary_plist_cannot_supply_entitlement_claims(self):
        for claims in ([], [packaging.FSMODULE], True):
            with self.subTest(claims=claims), mock.patch.object(packaging, "run", return_value=(plistlib.dumps(claims), b"")), self.assertRaisesRegex(ValueError, "dictionary"):
                packaging.signature_entitlements(pathlib.Path("fixture.appex"))

    def test_failed_display_command_is_not_retried_with_weaker_options(self):
        with mock.patch.object(packaging, "run", side_effect=ValueError("codesign failed")) as run, self.assertRaisesRegex(ValueError, "codesign failed"):
            packaging.signature_entitlements(pathlib.Path("fixture.appex"))
        self.assertEqual(run.call_count, 1)


class SignedCertificateExtractionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="reporeach-signed-fskit-test-")
        self.addCleanup(self.temporary.cleanup)
        self.app = pathlib.Path(self.temporary.name) / "RepoReach.app"
        self.module = self.app / packaging.MODULE_PATH
        (self.module / "Contents").mkdir(parents=True)
        (self.module / "Contents/embedded.provisionprofile").write_bytes(b"fixture, not a profile")
        self.claims = {packaging.FSMODULE: True, packaging.SANDBOX: True, packaging.APP_GROUPS: ["FIXTURE123.rr"]}
        self.certificate = certificate_shape([("1.2.840.113635.100.6.1.13", b"\x05\0")])
        for bundle in (self.app, self.module):
            (bundle / "Contents/Info.plist").write_bytes(plistlib.dumps({packaging.GROUP_INFO: "FIXTURE123.rr"}))

    def display(self, *args):
        if args[2] == "--verbose=4":
            return b"", b"Authority=Developer ID Application: Fixture\nTeamIdentifier=FIXTURE123\nflags=10000(runtime)\nTimestamp=fixture\n"
        if args[2] == "--entitlements":
            self.assertIn("--xml", args)
            path = pathlib.Path(args[-1])
            if path == self.module:
                return plistlib.dumps(self.claims), b""
            if path == self.app or path.name == "artifact-fs":
                return plistlib.dumps({packaging.APP_GROUPS: ["FIXTURE123.rr"]}), b""
            return b"", b""
        if args[2].startswith("--extract-certificates="):
            # An optional long-option argument must be attached; otherwise
            # codesign treats the destination prefix as another signed path.
            prefix = args[2].split("=", 1)[1]
            pathlib.Path(prefix + "0").write_bytes(self.certificate)
            return b"", b""
        self.assertEqual(args[:3], ("codesign", "--verify", "--strict"))
        return b"", b""

    def test_certificate_and_claims_remain_bound_to_profile_authorization(self):
        profile = {"fixture": True}
        with mock.patch.object(packaging, "check_bundle", return_value=self.module), mock.patch.object(packaging, "run", side_effect=self.display), mock.patch.object(packaging, "decode_profile", return_value=profile), mock.patch.object(packaging, "authorize_profile") as authorize:
            packaging.signed(self.app, "arm64", dict(self.claims))
        authorize.assert_called_once_with(profile, packaging.MODULE_ID, self.claims, certificate=self.certificate, team="FIXTURE123", require_bound=True)

    def test_missing_signed_capability_is_rejected_before_profile_authorization(self):
        expected = dict(self.claims)
        del self.claims[packaging.FSMODULE]
        with mock.patch.object(packaging, "check_bundle", return_value=self.module), mock.patch.object(packaging, "run", side_effect=self.display), mock.patch.object(packaging, "authorize_profile") as authorize, self.assertRaisesRegex(ValueError, "omits configured"):
            packaging.signed(self.app, "arm64", expected)
        authorize.assert_not_called()

    def test_actual_module_cannot_claim_extra_network_debug_or_security_access(self):
        expected = dict(self.claims)
        for key in ("com.apple.security.network.client", "com.apple.security.network.server", "get-task-allow", "com.apple.security.get-task-allow", "com.apple.security.files.user-selected.read-write"):
            self.claims[key] = True
            with self.subTest(key=key), mock.patch.object(packaging, "check_bundle", return_value=self.module), mock.patch.object(packaging, "run", side_effect=self.display), mock.patch.object(packaging, "authorize_profile") as authorize, self.assertRaisesRegex(ValueError, "unexpected entitlement"):
                packaging.signed(self.app, "arm64", expected)
            authorize.assert_not_called()
            del self.claims[key]

    def test_group_membership_and_certificate_must_match_for_every_ipc_participant(self):
        expected_group = {packaging.APP_GROUPS: ["FIXTURE123.rr"]}
        for component in (self.app / "Contents/Helpers/artifact-fs", self.app):
            for claims in ({}, {packaging.APP_GROUPS: ["OTHERTEAM1.rr"]}, {packaging.APP_GROUPS: [packaging.GROUP_TEMPLATE]}, dict(expected_group, **{packaging.SANDBOX: True})):
                def entitlements(path, allow_empty=False):
                    return claims if path == component else (self.claims if path == self.module else expected_group)
                with self.subTest(component=str(component), claims=claims), mock.patch.object(packaging, "check_bundle", return_value=self.module), mock.patch.object(packaging, "signing_identity", return_value=("FIXTURE123", self.certificate)), mock.patch.object(packaging, "signature_entitlements", side_effect=entitlements), mock.patch.object(packaging, "decode_profile"), mock.patch.object(packaging, "authorize_profile"), self.assertRaisesRegex(ValueError, "only their matching"):
                    packaging.signed(self.app, "arm64", dict(self.claims))
            for identity in (("OTHERTEAM1", self.certificate), ("FIXTURE123", b"another certificate")):
                def identity_for(path):
                    return identity if path == component else ("FIXTURE123", self.certificate)
                with self.subTest(component=str(component), identity=identity[0]), mock.patch.object(packaging, "check_bundle", return_value=self.module), mock.patch.object(packaging, "signing_identity", side_effect=identity_for), mock.patch.object(packaging, "signature_entitlements", side_effect=lambda path: self.claims if path == self.module else expected_group), mock.patch.object(packaging, "decode_profile"), mock.patch.object(packaging, "authorize_profile"), self.assertRaisesRegex(ValueError, "same actual signing"):
                    packaging.signed(self.app, "arm64", dict(self.claims))

    def test_nonparticipants_cannot_receive_the_ipc_group(self):
        group = {packaging.APP_GROUPS: ["FIXTURE123.rr"]}
        for excluded in (self.app / "Contents/Helpers/gh", self.app / "Contents/PlugIns/RepoReachFinder.appex"):
            def entitlements(path, allow_empty=False):
                if path == self.module:
                    return self.claims
                if path in (self.app, self.app / "Contents/Helpers/artifact-fs", excluded):
                    return group
                return {}
            with self.subTest(component=str(excluded)), mock.patch.object(packaging, "check_bundle", return_value=self.module), mock.patch.object(packaging, "signing_identity", return_value=("FIXTURE123", self.certificate)), mock.patch.object(packaging, "signature_entitlements", side_effect=entitlements), mock.patch.object(packaging, "decode_profile"), mock.patch.object(packaging, "authorize_profile"), self.assertRaisesRegex(ValueError, "must not claim"):
                packaging.signed(self.app, "arm64", dict(self.claims))


class CompiledBundleTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="reporeach-compiled-fskit-test-")
        self.addCleanup(self.temporary.cleanup)
        self.app = pathlib.Path(self.temporary.name) / "RepoReach.app"
        self.module = self.app / packaging.MODULE_PATH
        self.app_info = {"CFBundleIdentifier": "com.enoughtools.reporeach", "CFBundleExecutable": "RepoReach", packaging.GROUP_INFO: ".rr"}
        self.module_info = {
            "CFBundleIdentifier": packaging.MODULE_ID,
            "CFBundleExecutable": "RepoReachFSKit", "LSMinimumSystemVersion": "26.0", packaging.GROUP_INFO: ".rr",
            "EXAppExtensionAttributes": {
                "EXExtensionPointIdentifier": "com.apple.fskit.fsmodule",
                "FSActivateOptionSyntax": {"shortOptions": "o:"},
            },
        }
        self.entitlements = {packaging.FSMODULE: True, packaging.SANDBOX: True, packaging.APP_GROUPS: [packaging.GROUP_TEMPLATE]}
        for bundle, info in ((self.app, self.app_info), (self.module, self.module_info)):
            (bundle / "Contents/MacOS").mkdir(parents=True)
            (bundle / "Contents/MacOS" / info["CFBundleExecutable"]).write_bytes(b"compiled binary fixture\0\xff")
            (bundle / "Contents/Info.plist").write_bytes(plistlib.dumps(info))

    def check(self):
        (self.module / "Contents/Info.plist").write_bytes(plistlib.dumps(self.module_info))
        with mock.patch.object(packaging, "run", return_value=(b"arm64", b"")):
            return packaging.check_bundle(self.app, "arm64", self.entitlements)

    def test_accepts_compiled_mount_only_module_with_common_options(self):
        self.assertEqual(self.check(), self.module)

    def test_unresolved_group_is_compile_only_and_signed_metadata_must_match(self):
        self.assertEqual(self.check(), self.module)
        with self.assertRaisesRegex(ValueError, "unresolved"):
            packaging.check_group_metadata(self.app)
        self.assertEqual(packaging.configure_app_group(self.app, "FIXTURE123"), "FIXTURE123.rr")
        self.assertEqual(packaging.check_group_metadata(self.app, team="FIXTURE123"), "FIXTURE123.rr")
        with self.assertRaisesRegex(ValueError, "actual signing team"):
            packaging.check_group_metadata(self.app, team="OTHERTEAM1")
        self.module_info[packaging.GROUP_INFO] = "OTHERTEAM1.rr"
        with self.assertRaisesRegex(ValueError, "same app group"):
            self.check()

    def test_compiled_group_cannot_be_missing_malformed_or_silently_reassigned(self):
        for group in (None, "rr", "group.example", "FIXTURE123.*", "FIXTURE123.rr/path"):
            self.module_info[packaging.GROUP_INFO] = group
            self.app_info[packaging.GROUP_INFO] = group
            if group is None:
                self.module_info.pop(packaging.GROUP_INFO)
                self.app_info.pop(packaging.GROUP_INFO)
            (self.app / "Contents/Info.plist").write_bytes(plistlib.dumps(self.app_info))
            with self.subTest(group=group), self.assertRaises(ValueError):
                self.check()
        for bundle in (self.app, self.module):
            path = bundle / "Contents/Info.plist"
            info = plistlib.loads(path.read_bytes())
            info[packaging.GROUP_INFO] = "OTHERTEAM1.rr"
            path.write_bytes(plistlib.dumps(info))
        before = [(bundle / "Contents/Info.plist").read_bytes() for bundle in (self.app, self.module)]
        with self.assertRaisesRegex(ValueError, "another developer team"):
            packaging.configure_app_group(self.app, "FIXTURE123")
        self.assertEqual(before, [(bundle / "Contents/Info.plist").read_bytes() for bundle in (self.app, self.module)])

    def test_source_module_cannot_add_unrelated_entitlements(self):
        for key in ("com.apple.security.network.client", "com.apple.security.network.server", "com.apple.security.files.user-selected.read-write"):
            self.entitlements[key] = True
            with self.subTest(key=key), self.assertRaisesRegex(ValueError, "only its capability"):
                self.check()
            del self.entitlements[key]

    def test_preparation_binds_copied_runtime_metadata_and_all_claims_to_verified_signer(self):
        folder = pathlib.Path(self.temporary.name)
        module_source = folder / "module-template"
        module_source.write_bytes(plistlib.dumps(self.entitlements))
        participant_source = folder / "participant-template"
        participant_source.write_bytes(plistlib.dumps({packaging.APP_GROUPS: [packaging.GROUP_TEMPLATE]}))
        args = SimpleNamespace(app=self.app, signing_component=self.app / "Contents/Helpers/gh", profile=folder / "profile", bundle_id=packaging.MODULE_ID, entitlements=module_source, app_entitlements=participant_source, helper_entitlements=participant_source, output_directory=folder / "claims")
        resolved = packaging.resolve_entitlements(self.entitlements, "FIXTURE123")
        with mock.patch.object(packaging, "signing_identity", return_value=("FIXTURE123", b"selected certificate")) as identity, mock.patch.object(packaging, "decode_profile", return_value={"profile": "authenticated"}), mock.patch.object(packaging, "authorize_profile", return_value=resolved) as authorize:
            packaging.prepare_signing(args)
        identity.assert_called_once_with(args.signing_component)
        authorize.assert_called_once_with({"profile": "authenticated"}, packaging.MODULE_ID, self.entitlements, certificate=b"selected certificate", team="FIXTURE123")
        self.assertEqual(packaging.check_group_metadata(self.app, team="FIXTURE123"), "FIXTURE123.rr")
        self.assertEqual(plistlib.loads((args.output_directory / "module.entitlements").read_bytes()), resolved)
        for name in ("app", "helper"):
            self.assertEqual(plistlib.loads((args.output_directory / (name + ".entitlements")).read_bytes()), {packaging.APP_GROUPS: ["FIXTURE123.rr"]})
        self.assertEqual(plistlib.loads(module_source.read_bytes()), self.entitlements)
        self.assertEqual(plistlib.loads(participant_source.read_bytes()), {packaging.APP_GROUPS: [packaging.GROUP_TEMPLATE]})

    def test_missing_activation_metadata_is_rejected_before_native_tools(self):
        del self.module_info["EXAppExtensionAttributes"]["FSActivateOptionSyntax"]
        with mock.patch.object(packaging, "run") as run, self.assertRaisesRegex(ValueError, "mount activation"):
            # The previously compiled app identified itself as a filesystem but
            # mount refused it before calling loadResource or activate.
            (self.module / "Contents/Info.plist").write_bytes(plistlib.dumps(self.module_info))
            packaging.check_bundle(self.app, "arm64", self.entitlements)
        run.assert_not_called()

    def test_rejects_invalid_or_unimplemented_activation_options(self):
        invalid = (False, [], {}, {"shortOptions": ""}, {"shortOptions": "u:g:m:"}, {"shortOptions": "o:", "longOptions": {"unsupported": True}})
        for syntax in invalid:
            with self.subTest(syntax=syntax):
                self.module_info["EXAppExtensionAttributes"]["FSActivateOptionSyntax"] = syntax
                with self.assertRaisesRegex(ValueError, "mount activation"):
                    self.check()

    def test_rejects_unimplemented_checking_and_formatting_advertisements(self):
        for key in ("FSCheckOptionSyntax", "FSFormatOptionSyntax"):
            with self.subTest(key=key):
                self.module_info["EXAppExtensionAttributes"][key] = {}
                with self.assertRaisesRegex(ValueError, "unsupported checking or formatting"):
                    self.check()
                del self.module_info["EXAppExtensionAttributes"][key]


class LocalValidationArtifactTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="reporeach-validation-export-test-")
        self.addCleanup(self.temporary.cleanup)
        self.folder = pathlib.Path(self.temporary.name)
        self.app = self.folder / "stage/RepoReach.app"
        self.module = self.app / "Contents/Extensions/RepoReachFSKit.appex"
        self.module.mkdir(parents=True)
        (self.app / "Contents/Resources").mkdir()
        helpers = self.app / "Contents/Helpers"
        helpers.mkdir()
        for name in ("artifact-fs", "gh"):
            (helpers / name).write_bytes(b"binary fixture\0\xff")
        self.source = {"url": "https://github.com/enoughtools/reporeach", "dirty": False, "revision": "a" * 40, "contentSha256": "b" * 64}
        self.source_path = self.folder / "source.json"
        self.source_path.write_text(json.dumps(self.source))
        self.args = SimpleNamespace(app=self.app, arch="arm64", source_file=self.source_path, output=self.folder / "products")

    def run_fixture(self, *arguments):
        if arguments[0] == "lipo":
            return "arm64"
        if arguments[0] == "ditto":
            # Exercise the exported metadata against real archive bytes without
            # requiring Apple tooling for Linux's distribution-guard CI tests.
            app, archive = pathlib.Path(arguments[-2]), pathlib.Path(arguments[-1])
            with zipfile.ZipFile(archive, "w") as output:
                for path in app.rglob("*"):
                    if path.is_file():
                        output.write(path, path.relative_to(app.parent))
            return ""
        return "fixture toolchain"

    def export(self):
        with mock.patch.object(local_validation.sys, "platform", "darwin"), mock.patch.object(local_validation, "run", side_effect=self.run_fixture):
            local_validation.export(self.args)

    def test_metadata_hashes_the_actual_unprovisioned_app_archive(self):
        self.export()
        metadata = json.loads((self.args.output / "RepoReach-local-validation-arm64.json").read_text())
        self.assertFalse(metadata["distribution"])
        self.assertFalse(metadata["extensionActivationAuthorized"])
        self.assertFalse(metadata["mountedValidationPassed"])
        self.assertTrue(metadata["localSigningRequired"])
        self.assertEqual(metadata["source"], self.source)
        archive = self.args.output / metadata["artifact"]["filename"]
        self.assertEqual(metadata["artifact"]["sha256"], local_validation.digest(archive))
        self.assertEqual(metadata["artifact"]["bytes"], archive.stat().st_size)
        with zipfile.ZipFile(archive) as contents:
            self.assertEqual(contents.read("RepoReach.app/Contents/Helpers/artifact-fs"), b"binary fixture\0\xff")
            self.assertIn(b"not a release", contents.read("RepoReach.app/Contents/Resources/LocalValidation.txt"))
        with self.assertRaisesRegex(ValueError, "overwrite"):
            self.export()

    def test_rejects_embedded_profiles_and_missing_helpers(self):
        profile = self.module / "Contents/embedded.provisionprofile"
        profile.parent.mkdir()
        profile.write_bytes(b"fixture, not a profile")
        with self.assertRaisesRegex(ValueError, "provisioning profile"):
            self.export()
        profile.unlink()
        (self.app / "Contents/Helpers/artifact-fs").unlink()
        with self.assertRaisesRegex(ValueError, "helper is missing"):
            self.export()
        self.assertFalse(self.args.output.exists())

    def test_rejects_uncommitted_or_unidentifiable_source(self):
        for change in ({"dirty": True}, {"revision": ""}, {"contentSha256": ""}, {"url": "https://example.com/other"}):
            with self.subTest(change=change):
                self.source_path.write_text(json.dumps(dict(self.source, **change)))
                with self.assertRaises(ValueError):
                    self.export()
        self.assertFalse(self.args.output.exists())

    def test_rejects_helper_architecture_mismatch(self):
        with mock.patch.object(local_validation.sys, "platform", "darwin"), mock.patch.object(local_validation, "run", return_value="x86_64"):
            with self.assertRaisesRegex(ValueError, "architecture"):
                local_validation.export(self.args)
        self.assertFalse(self.args.output.exists())


class ValidationGenerationFlagsTests(unittest.TestCase):
    def test_generation_override_requires_explicit_local_export_and_safe_absolute_path(self):
        script = pathlib.Path(__file__).with_name("build-macos.sh")
        root = script.resolve().parents[1]
        generation = root / "build/fskit-validation/generations/flag-fixture"
        cases = (
            (["--validation-root"], "requires an absolute"),
            (["--validation-root", ""], "requires an absolute"),
            (["--compile-only", "--validation-root", str(generation)], "only valid with"),
            (["--validation-artifact", "--validation-root", "relative/generation"], "normalized absolute"),
            (["--validation-artifact", "--validation-root", "/"], "normalized absolute"),
            (["--validation-artifact", "--validation-root", str(root / "build/fskit-validation")], "normalized absolute"),
            (["--validation-artifact", "--validation-root", str(generation) + "/../other"], "normalized absolute"),
        )
        for arguments, expected in cases:
            with self.subTest(arguments=arguments):
                result = subprocess.run(["bash", str(script), *arguments], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected, result.stderr)
                self.assertEqual(result.stdout, "")
        self.assertFalse(generation.exists())


class BuildRegistrationCleanupTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="reporeach-registration-cleanup-test-")
        self.addCleanup(self.temporary.cleanup)
        self.app = pathlib.Path(self.temporary.name) / "derived/Build/Products/Release/RepoReach.app"
        self.app.mkdir(parents=True)
        self.module = self.app / "Contents/Extensions/RepoReachFSKit.appex"
        self.inspector = self.app.parents[3] / "inspect-built-app-registration"
        self.script = pathlib.Path(__file__).with_name("build-macos.sh").read_text()
        self.code = self.script.split("<<'PY_REGISTRATION'\n", 1)[1].split("\nPY_REGISTRATION", 1)[0]
        self.no_modules = b" (no matches)\n"

    def result(self, output=b"", status=0):
        return SimpleNamespace(returncode=status, stdout=output, stderr=b"")

    def parent(self, registered=False):
        return self.result(json.dumps({
            "ok": True, "bundle_identifier": "com.enoughtools.reporeach",
            "expected_app_path": str(self.app), "exact_app_registered": registered,
            "application_count": 3, "unknowns": [], "output_truncated": False,
        }).encode())

    def run_cleanup(self, results, backend="fskit"):
        with mock.patch.object(sys, "argv", ["cleanup", str(self.app), backend, str(self.inspector)]), mock.patch.object(subprocess, "run", side_effect=results) as run:
            exec(compile(self.code, "build-macos.sh registration cleanup", "exec"), {})
        return run.call_args_list

    def inventory(self, module):
        return f"     com.enoughtools.reporeach.fskit((null))\tfixture-uuid\t2026-10-05 00:00:00 +0000\t{module}\n (1 plug-in)\n".encode()

    def test_cleanup_unregisters_only_exact_successful_product_and_requires_fresh_proof(self):
        calls = self.run_cleanup([self.parent(True), self.result(self.inventory(self.module)), self.result(), self.parent(), self.result(self.no_modules)])
        self.assertEqual(calls[0].args[0], [str(self.inspector), str(self.app)])
        self.assertEqual(calls[1].args[0], ["/usr/bin/pluginkit", "-m", "-A", "-D", "-v", "-i", packaging.MODULE_ID])
        self.assertEqual(calls[2].args[0], ["/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister", "-u", str(self.app)])
        self.assertEqual(calls[3].args[0], calls[0].args[0])
        self.assertEqual(calls[4].args[0], calls[1].args[0])
        self.assertTrue(all(call.kwargs["timeout"] == 20 for call in calls))
        self.assertNotIn("REGISTER_APP_WITH_LAUNCH_SERVICES", self.script)
        compile_position = self.script.index("<<'PY_INSPECTOR'")
        build_position = self.script.index("CODE_SIGNING_ALLOWED=NO CODE_SIGNING_REQUIRED=NO build")
        cleanup_position = self.script.index("<<'PY_REGISTRATION'")
        validation_position = self.script.index('python3 "$ROOT/scripts/validate-fskit-bundle.py" compile')
        self.assertLess(compile_position, build_position)
        self.assertLess(build_position, cleanup_position)
        self.assertLess(cleanup_position, validation_position)
        self.assertNotIn("trap ", self.script[:cleanup_position])

    def test_already_absent_exact_parent_and_module_skip_unregister_preserving_other_apps(self):
        other = self.app.parent / "Other.app/Contents/Extensions/RepoReachFSKit.appex"
        calls = self.run_cleanup([self.parent(), self.result(self.inventory(other))])
        self.assertEqual(len(calls), 2)
        self.assertFalse(any("-u" in call.args[0] for call in calls))
        self.assertTrue(all(str(other) not in call.args[0] for call in calls))

    def test_nonzero_unregister_is_accepted_only_with_fresh_parent_and_module_absence(self):
        for status in (1, 7, 127):
            with self.subTest(status=status):
                calls = self.run_cleanup([self.parent(True), self.result(self.no_modules), self.result(status=status), self.parent(), self.result(self.no_modules)])
                self.assertEqual(len(calls), 5)
        for parent, modules in ((True, self.no_modules), (False, self.inventory(self.module))):
            with self.subTest(parent=parent), self.assertRaisesRegex(SystemExit, "still registered"):
                self.run_cleanup([self.parent(True), self.result(self.no_modules), self.result(status=1), self.parent(parent), self.result(modules)])
        with self.assertRaisesRegex(SystemExit, "Could not inspect"):
            self.run_cleanup([self.parent(True), self.result(self.no_modules), self.result(status=1), self.result(status=1)])

    def test_incomplete_module_inventory_stops_before_unregister(self):
        for inventory, reason in ((b"", "Incomplete"), (b" (2 plug-ins)\n", "Incomplete"), (b"unstructured result\n", "Unexpected"), (b" (no matches)\n (no matches)\n", "Unexpected")):
            with self.subTest(inventory=inventory), self.assertRaisesRegex(SystemExit, reason):
                self.run_cleanup([self.parent(), self.result(inventory)])

    def test_parent_inventory_requires_complete_fixed_identity_and_typed_absence_report(self):
        valid = json.loads(self.parent().stdout)
        changes = ({"ok": False}, {"bundle_identifier": "another.app"}, {"expected_app_path": "/another/RepoReach.app"},
                   {"exact_app_registered": 0}, {"application_count": True}, {"application_count": 257},
                   {"unknowns": ["unavailable"]}, {"output_truncated": True},
                   {"exact_app_registered": True, "application_count": 0})
        for change in changes:
            with self.subTest(change=change), self.assertRaisesRegex(SystemExit, "Incomplete"):
                self.run_cleanup([self.result(json.dumps(dict(valid, **change)).encode())])
        for output in (b"", b"not JSON", b"[]", json.dumps({key: value for key, value in valid.items() if key != "exact_app_registered"}).encode()):
            with self.subTest(output=output), self.assertRaises(SystemExit):
                self.run_cleanup([self.result(output)])

    def test_tool_errors_or_timeouts_stop_without_cleanup_mutation(self):
        with self.assertRaisesRegex(SystemExit, "Could not inspect"):
            self.run_cleanup([self.result(status=1)])
        with self.assertRaisesRegex(SystemExit, "Could not verify"):
            self.run_cleanup([self.parent(), self.result(status=1)])
        with self.assertRaisesRegex(SystemExit, "could not finish"):
            self.run_cleanup([subprocess.TimeoutExpired("fixture-inspector", 20)])

    def test_legacy_parent_requires_public_absence_and_redirected_product_is_rejected(self):
        self.assertEqual(len(self.run_cleanup([self.parent()], backend="macfuse")), 1)
        calls = self.run_cleanup([self.parent(True), self.result(), self.parent()], backend="macfuse")
        self.assertEqual(len(calls), 3)
        self.assertTrue(all("pluginkit" not in " ".join(call.args[0]) for call in calls))
        with self.assertRaisesRegex(SystemExit, "still registered"):
            self.run_cleanup([self.parent(True), self.result(), self.parent(True)], backend="macfuse")
        self.app.rmdir()
        target = pathlib.Path(self.temporary.name) / "installed-app"
        target.mkdir()
        self.app.symlink_to(target, target_is_directory=True)
        with mock.patch.object(sys, "argv", ["cleanup", str(self.app), "fskit", str(self.inspector)]), mock.patch.object(subprocess, "run") as run, self.assertRaisesRegex(SystemExit, "symlink"):
            exec(compile(self.code, "build-macos.sh registration cleanup", "exec"), {})
        run.assert_not_called()

    @unittest.skipUnless(sys.platform == "darwin", "Compiles and reads the public macOS registration API")
    def test_public_inspector_compiles_on_selected_sdk_and_reports_exact_absence(self):
        import platform
        source = pathlib.Path(__file__).resolve().parents[1] / "native/Tools/InspectBuiltAppRegistration.swift"
        subprocess.run(["xcrun", "--sdk", "macosx", "swiftc", "-parse-as-library", "-swift-version", "6", "-strict-concurrency=complete", "-warnings-as-errors", "-target", platform.machine() + "-apple-macosx13.0", "-framework", "AppKit", str(source), "-o", str(self.inspector)], check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
        result = subprocess.run([str(self.inspector), str(self.app)], check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=20)
        report = json.loads(result.stdout)
        self.assertEqual(set(report), set(json.loads(self.parent().stdout)))
        self.assertIs(report["ok"], True)
        self.assertEqual(report["bundle_identifier"], "com.enoughtools.reporeach")
        self.assertEqual(report["expected_app_path"], str(self.app))
        self.assertIs(report["exact_app_registered"], False)
        self.assertEqual(report["unknowns"], [])
        self.assertIs(report["output_truncated"], False)
        for path in ("relative/RepoReach.app", "/tmp/../RepoReach.app", "/tmp/./RepoReach.app", "/tmp//RepoReach.app", "/tmp/RepoReach.app/"):
            with self.subTest(path=path):
                rejected = subprocess.run([str(self.inspector), path], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=20)
                self.assertEqual(rejected.returncode, 1)
                self.assertEqual(json.loads(rejected.stdout)["unknowns"], ["invalid_expected_app_path"])
        text = source.read_text()
        self.assertNotIn("standardizedFileURL", text)
        self.assertNotIn("resolvingSymlinks", text)
        self.assertNotIn("URL(fileURLWithPath", text)


if __name__ == "__main__":
    unittest.main()
