#!/usr/bin/env python3
"""Regression checks for distribution authorization; fixtures are not profiles."""
import copy
import datetime
import importlib.util
import pathlib
import plistlib
import sys
import tempfile
import unittest

source = pathlib.Path(__file__).with_name("validate-fskit-bundle.py")
spec = importlib.util.spec_from_file_location("fskit_packaging", source)
packaging = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packaging)


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


if __name__ == "__main__":
    unittest.main()
