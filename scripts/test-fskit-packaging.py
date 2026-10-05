#!/usr/bin/env python3
"""Regression checks for distribution authorization; fixtures are not profiles."""
import copy
import datetime
import importlib.util
import json
import pathlib
import plistlib
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
        self.team = "FIXTURE1234"
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


if __name__ == "__main__":
    unittest.main()
