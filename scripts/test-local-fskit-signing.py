#!/usr/bin/env python3
"""Security regressions for isolated local signing; never use signing material."""
import hashlib
import importlib.util
import json
import pathlib
import plistlib
import stat
import subprocess
import tempfile
import unittest
import zipfile
from types import SimpleNamespace
from unittest import mock

spec = importlib.util.spec_from_file_location("local_signing", pathlib.Path(__file__).with_name("sign-fskit-validation.py"))
signing = importlib.util.module_from_spec(spec)
spec.loader.exec_module(signing)


def member(name, mode=stat.S_IFREG | 0o644):
    item = zipfile.ZipInfo(name)
    item.create_system = 3
    item.external_attr = mode << 16
    item.compress_type = zipfile.ZIP_DEFLATED
    return item


class SigningFixtures(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="reporeach-local-signing-test-")
        self.addCleanup(self.temporary.cleanup)
        self.folder = pathlib.Path(self.temporary.name)
        self.archive = self.folder / "RepoReach-local-validation-arm64.zip"
        self.metadata = self.folder / "RepoReach-local-validation-arm64.json"
        self.profile = self.folder / "fixture-profile"
        self.profile.write_bytes(b"fixture bytes, not a provisioning profile")
        self.files = {
            "RepoReach.app/Contents/Resources/LocalValidation.txt": signing.MARKER.encode(),
            "RepoReach.app/Contents/Helpers/artifact-fs": b"engine\0\xff",
            "RepoReach.app/Contents/Helpers/gh": b"gh\0\xff",
        }
        for prefix, identifier, name in (
            ("RepoReach.app", "com.enoughtools.reporeach", "RepoReach"),
            ("RepoReach.app/" + signing.packaging.MODULE_PATH, signing.packaging.MODULE_ID, "RepoReachFSKit"),
            ("RepoReach.app/Contents/PlugIns/RepoReachFinder.appex", "com.enoughtools.reporeach.finder", "RepoReachFinder"),
        ):
            self.files[prefix + "/Contents/Info.plist"] = plistlib.dumps({
                "CFBundleIdentifier": identifier, "CFBundleExecutable": name,
                "LSMinimumSystemVersion": "26.0",
                **({signing.packaging.GROUP_INFO: ".rr"} if identifier != "com.enoughtools.reporeach.finder" else {}),
                "EXAppExtensionAttributes": {
                    "EXExtensionPointIdentifier": "com.apple.fskit.fsmodule",
                    "FSActivateOptionSyntax": {"shortOptions": "o:"},
                },
            })
            self.files[prefix + "/Contents/MacOS/" + name] = b"binary fixture\0\xff"
        self.args = SimpleNamespace(
            archive=self.archive, metadata=self.metadata, archive_sha256="",
            source_revision="a" * 40, source_sha256="b" * 64, workflow_run=123,
            arch="arm64", identity="C" * 40, team="FIXTURE123", profile=self.profile,
        )
        self.source = {"url": signing.SOURCE_URL, "revision": self.args.source_revision, "dirty": False, "contentSha256": self.args.source_sha256}
        self.write_archive()

    def write_archive(self, extra=()):
        with zipfile.ZipFile(self.archive, "w") as archive:
            for name, contents in self.files.items():
                executable = "/MacOS/" in name or "/Helpers/" in name
                archive.writestr(member(name, stat.S_IFREG | (0o755 if executable else 0o644)), contents)
            for info, contents in extra:
                archive.writestr(info, contents)
        self.args.archive_sha256 = signing.digest(self.archive)
        self.document = {
            "purpose": "local-fskit-validation", "distribution": False,
            "source": self.source, "architecture": "arm64", "filesystemBackend": "fskit",
            "localSigningRequired": True, "extensionActivationAuthorized": False,
            "mountedValidationPassed": False,
            "artifact": {"filename": self.archive.name, "bytes": self.archive.stat().st_size, "sha256": self.args.archive_sha256},
        }
        self.metadata.write_text(json.dumps(self.document))

    def extract(self):
        destination = self.folder / "extracted"
        destination.mkdir()
        return signing.extract_app(self.archive, destination)


class ExtractionTests(SigningFixtures):
    def test_binary_bytes_and_executable_modes_survive_without_appledouble(self):
        self.write_archive([(member("__MACOSX/RepoReach.app/._Contents"), b"resource metadata")])
        app = self.extract()
        self.assertEqual((app / "Contents/Helpers/artifact-fs").read_bytes(), b"engine\0\xff")
        self.assertTrue((app / "Contents/Helpers/artifact-fs").stat().st_mode & 0o111)
        self.assertFalse((app.parent / "__MACOSX").exists())

    def test_rejects_traversal_links_devices_and_privileged_modes_before_writing(self):
        bad = (
            member("../escaped"), member("/absolute"), member("RepoReach.app/Contents/../escaped"),
            member("RepoReach.app\\escaped"), member("Other.app/file"),
            member("RepoReach.app/linked", stat.S_IFLNK | 0o777),
            member("RepoReach.app/device", stat.S_IFCHR | 0o644),
            member("RepoReach.app/privileged", stat.S_IFREG | 0o4755),
        )
        for index, item in enumerate(bad):
            with self.subTest(path=item.filename):
                self.write_archive([(item, b"../../../escaped")])
                destination = self.folder / str(index)
                destination.mkdir()
                with self.assertRaises(ValueError):
                    signing.extract_app(self.archive, destination)
                self.assertEqual(list(destination.iterdir()), [])

    def test_rejects_case_unicode_and_file_parent_collisions(self):
        cases = (
            [(member("RepoReach.app/contents/other"), b"x")],
            [(member("RepoReach.app/Caf\u00e9/file"), b"x"), (member("RepoReach.app/Cafe\u0301/other"), b"y")],
            [(member("RepoReach.app/parent"), b"x"), (member("RepoReach.app/parent/child"), b"y")],
        )
        for index, entries in enumerate(cases):
            with self.subTest(case=index):
                self.write_archive(entries)
                destination = self.folder / str(index)
                destination.mkdir()
                with self.assertRaises(ValueError):
                    signing.extract_app(self.archive, destination)
                self.assertEqual(list(destination.iterdir()), [])

    def test_rejects_bounded_expansion_and_missing_validation_marker(self):
        destination = self.folder / "bounds"
        destination.mkdir()
        with mock.patch.object(signing, "MAX_EXPANDED", 1), self.assertRaisesRegex(ValueError, "limits"):
            signing.extract_app(self.archive, destination)
        self.assertEqual(list(destination.iterdir()), [])
        self.files["RepoReach.app/Contents/Resources/LocalValidation.txt"] = b"regular release"
        self.write_archive()
        with self.assertRaisesRegex(ValueError, "marker"):
            signing.extract_app(self.archive, destination)

    def test_bundle_plists_cannot_select_an_executable_outside_the_bundle(self):
        app = self.extract()
        info_path = app / "Contents/Info.plist"
        info = plistlib.loads(info_path.read_bytes())
        info["CFBundleExecutable"] = "/bin/ls"
        info_path.write_bytes(plistlib.dumps(info))
        with mock.patch.object(signing.packaging, "check_bundle") as check, self.assertRaisesRegex(ValueError, "Unsafe"):
            signing.components(app, "arm64", {})
        check.assert_not_called()


class ProvenanceTests(SigningFixtures):
    def test_independent_checksum_and_test_purpose_are_required(self):
        self.assertEqual(signing.verify_metadata(self.args), self.document)
        for change in ({"distribution": True}, {"mountedValidationPassed": True}, {"extensionActivationAuthorized": True}, {"architecture": "x86_64"}):
            with self.subTest(change=change):
                self.metadata.write_text(json.dumps(dict(self.document, **change)))
                with self.assertRaises(ValueError):
                    signing.verify_metadata(self.args)
        self.metadata.write_text(json.dumps(self.document))
        self.args.archive_sha256 = "e" * 64
        with self.assertRaisesRegex(ValueError, "SHA-256"):
            signing.verify_metadata(self.args)

    def test_workflow_must_be_successful_from_exact_repository_revision_and_artifact(self):
        workflow = {"repository": {"full_name": signing.REPOSITORY}, "head_sha": self.args.source_revision, "path": ".github/workflows/reporeach.yml", "event": "workflow_dispatch", "status": "completed", "conclusion": "success"}
        artifact = {"id": 456, "name": "fskit-local-validation-arm64", "expired": False, "workflow_run": {"head_sha": self.args.source_revision}}
        with mock.patch.object(signing, "run", side_effect=[json.dumps(workflow).encode(), json.dumps({"artifacts": [artifact]}).encode()]):
            self.assertEqual(signing.verify_workflow(self.args)["artifactId"], 456)
        for change in ({"head_sha": "d" * 40}, {"conclusion": "failure"}, {"event": "push"}, {"repository": {"full_name": "other/repo"}}):
            with self.subTest(change=change), mock.patch.object(signing, "run", return_value=json.dumps(dict(workflow, **change)).encode()), self.assertRaises(ValueError):
                signing.verify_workflow(self.args)
        with mock.patch.object(signing, "run", side_effect=[json.dumps(workflow).encode(), json.dumps({"artifacts": [dict(artifact, expired=True)]}).encode()]), self.assertRaises(ValueError):
            signing.verify_workflow(self.args)

    def test_source_marker_cannot_substitute_for_committed_fingerprint(self):
        responses = [b"git@github.com:enoughtools/reporeach.git", b"", self.args.source_revision.encode()]
        with mock.patch.object(signing, "run", side_effect=responses), mock.patch.object(signing, "committed_fingerprint", return_value="e" * 64), self.assertRaisesRegex(ValueError, "fingerprint"):
            signing.verify_source(self.args, self.document)
        dirty = [b"git@github.com:enoughtools/reporeach.git", b" M source.swift"]
        with mock.patch.object(signing, "run", side_effect=dirty), self.assertRaisesRegex(ValueError, "source changes"):
            signing.verify_source(self.args, self.document)

    def test_committed_fingerprint_reads_git_tree_without_worktree_changes(self):
        root = self.folder / "repository"
        root.mkdir()
        files = {"a.txt": b"a\0\xff", "folder/b.txt": b"b"}
        for name, contents in files.items():
            path = root / name
            path.parent.mkdir(exist_ok=True)
            path.write_bytes(contents)
        subprocess.run(["git", "init", "-q", str(root)], check=True)
        subprocess.run(["git", "-C", str(root), "add", "."], check=True)
        subprocess.run(["git", "-C", str(root), "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"], check=True)
        revision = subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD"], text=True).strip()
        expected = hashlib.sha256()
        for name, contents in sorted(files.items()):
            expected.update(name.encode() + b"\0" + hashlib.sha256(contents).digest())
        (root / "a.txt").write_bytes(b"uncommitted")
        with mock.patch.object(signing, "ROOT", root):
            self.assertEqual(signing.committed_fingerprint(revision), expected.hexdigest())


class SignatureInspectionTests(unittest.TestCase):
    def test_extracted_leaf_must_match_selected_identity(self):
        certificate = b"fixture public certificate, not signing material"
        fingerprint = hashlib.sha1(certificate).hexdigest().upper()
        details = b"Authority=Developer ID Application: Fixture\nTeamIdentifier=FIXTURE123\nflags=10000(runtime)\nTimestamp=fixture\n"
        result = SimpleNamespace(returncode=0, stdout=b"", stderr=details)
        with tempfile.TemporaryDirectory(prefix="reporeach-signature-test-") as folder:
            private = pathlib.Path(folder)
            path = private / "fixture.appex"

            def run(*args):
                if args[2].startswith("--extract-certificates="):
                    prefix = args[2].split("=", 1)[1]
                    self.assertEqual(pathlib.Path(prefix).parent, private)
                    pathlib.Path(prefix + "0").write_bytes(certificate)
                    return b""
                self.assertEqual(args[:3], ("codesign", "--verify", "--strict"))
                return b""

            with mock.patch.object(signing, "run", side_effect=run), mock.patch.object(signing.subprocess, "run", return_value=result):
                signing.verify_signature(path, fingerprint, "FIXTURE123", private)
                with self.assertRaisesRegex(ValueError, "different identity"):
                    signing.verify_signature(path, "0" * 40, "FIXTURE123", private)


class SigningBoundaryTests(SigningFixtures):
    def signing_context(self):
        requested = {signing.packaging.FSMODULE: True, signing.packaging.SANDBOX: True, signing.packaging.APP_GROUPS: [signing.packaging.GROUP_TEMPLATE]}
        calls = []
        self.captured_claims = {}

        def run(*arguments):
            calls.append(arguments)
            if arguments[0] == "git":
                if "FSKitExtension" in arguments[-1]:
                    value = requested
                elif "FinderExtension" in arguments[-1]:
                    value = {signing.packaging.SANDBOX: True}
                else:
                    value = {signing.packaging.APP_GROUPS: [signing.packaging.GROUP_TEMPLATE]}
                return plistlib.dumps(value)
            if arguments[0] == "lipo":
                return b"arm64"
            if arguments[0] == "codesign":
                if "--entitlements" in arguments:
                    path = pathlib.Path(arguments[arguments.index("--entitlements") + 1])
                    self.captured_claims[pathlib.Path(arguments[-1]).name] = plistlib.loads(path.read_bytes())
                else:
                    self.captured_claims[pathlib.Path(arguments[-1]).name] = {}
            return b""

        stack = __import__("contextlib").ExitStack()
        self.addCleanup(stack.close)
        for target, value in (("ROOT", self.folder), ("verify_source", mock.Mock(return_value=dict(self.source, revision="d" * 40))), ("verify_workflow", mock.Mock(return_value={"url": "fixture", "artifactId": 456})), ("identity_certificate", mock.Mock(return_value=b"fixture certificate")), ("run", mock.Mock(side_effect=run)), ("verify_signature", mock.Mock())):
            stack.enter_context(mock.patch.object(signing, target, value))
        stack.enter_context(mock.patch.object(signing.sys, "platform", "darwin"))
        stack.enter_context(mock.patch.object(signing.packaging, "run", return_value=(b"arm64", b"")))
        stack.enter_context(mock.patch.object(signing.packaging, "decode_profile", return_value={"fixture": True}))
        authorize = stack.enter_context(mock.patch.object(signing.packaging, "authorize_profile", return_value=signing.packaging.resolve_entitlements(requested, self.args.team)))
        signed = stack.enter_context(mock.patch.object(signing.packaging, "signed"))
        return calls, authorize, signed

    def test_only_verified_profile_bound_components_are_published_in_isolated_output(self):
        calls, authorize, signed = self.signing_context()
        signing.sign(self.args)
        authorize.assert_called_once_with({"fixture": True}, signing.packaging.MODULE_ID, {signing.packaging.FSMODULE: True, signing.packaging.SANDBOX: True, signing.packaging.APP_GROUPS: [self.args.team + ".rr"]}, certificate=b"fixture certificate", team=self.args.team)
        order = [pathlib.Path(call[-1]).name for call in calls if call[0] == "codesign"]
        self.assertEqual(order, ["artifact-fs", "gh", "RepoReachFSKit.appex", "RepoReachFinder.appex", "RepoReach.app"])
        self.assertEqual(signing.verify_signature.call_count, 5)
        group = {signing.packaging.APP_GROUPS: [self.args.team + ".rr"]}
        self.assertEqual(self.captured_claims["artifact-fs"], group)
        self.assertEqual(self.captured_claims["RepoReach.app"], group)
        self.assertEqual(self.captured_claims["RepoReachFSKit.appex"][signing.packaging.APP_GROUPS], group[signing.packaging.APP_GROUPS])
        self.assertEqual(self.captured_claims["gh"], {})
        self.assertNotIn(signing.packaging.APP_GROUPS, self.captured_claims["RepoReachFinder.appex"])
        signed.assert_called_once()
        products = list((self.folder / "build/fskit-validation/signed").glob("arm64-*"))
        self.assertEqual(len(products), 1)
        metadata = json.loads((products[0] / "local-signing.json").read_text())
        self.assertEqual(metadata["source"], self.source)
        self.assertEqual(metadata["signingUtilitySource"]["revision"], "d" * 40)
        self.assertFalse(metadata["distribution"])
        self.assertFalse(metadata["mountedValidationPassed"])
        self.assertFalse(metadata["extensionActivationAuthorized"])
        self.assertEqual(metadata["appGroupIdentifier"], self.args.team + ".rr")
        self.assertEqual(signing.digest(self.archive), self.args.archive_sha256)
        module = products[0] / "RepoReach.app" / signing.packaging.MODULE_PATH
        self.assertEqual((module / "Contents/embedded.provisionprofile").read_bytes(), self.profile.read_bytes())
        for bundle in (products[0] / "RepoReach.app", module):
            self.assertEqual(plistlib.loads((bundle / "Contents/Info.plist").read_bytes())[signing.packaging.GROUP_INFO], self.args.team + ".rr")
        with zipfile.ZipFile(self.archive) as archive:
            self.assertEqual(plistlib.loads(archive.read("RepoReach.app/Contents/Info.plist"))[signing.packaging.GROUP_INFO], ".rr")
        with self.assertRaisesRegex(ValueError, "overwrite"):
            signing.sign(self.args)

    def test_unauthorized_profile_prevents_any_signing_or_retained_app(self):
        calls, authorize, _ = self.signing_context()
        authorize.side_effect = ValueError("certificate is not authorized")
        with self.assertRaisesRegex(ValueError, "not authorized"):
            signing.sign(self.args)
        self.assertFalse(any(call[0] == "codesign" for call in calls))
        self.assertEqual(list((self.folder / "build/fskit-validation/signed").iterdir()), [])

    def test_input_without_group_metadata_cannot_gain_new_ipc_claims(self):
        info_path = "RepoReach.app/" + signing.packaging.MODULE_PATH + "/Contents/Info.plist"
        info = plistlib.loads(self.files[info_path])
        del info[signing.packaging.GROUP_INFO]
        self.files[info_path] = plistlib.dumps(info)
        self.write_archive()
        calls, _, _ = self.signing_context()
        with self.assertRaisesRegex(ValueError, "same app group"):
            signing.sign(self.args)
        self.assertFalse(any(call[0] == "codesign" for call in calls))
        self.assertEqual(list((self.folder / "build/fskit-validation/signed").iterdir()), [])

    def test_participant_source_cannot_add_network_or_foreign_group_claims(self):
        for template in ({signing.packaging.APP_GROUPS: ["OTHERTEAM1.rr"]}, {signing.packaging.APP_GROUPS: [signing.packaging.GROUP_TEMPLATE], "com.apple.security.network.client": True}, {}):
            with self.subTest(template=template), mock.patch.object(signing, "run", return_value=plistlib.dumps(template)), self.assertRaisesRegex(ValueError, "only the Team-prefix"):
                signing.participant_entitlements(self.args.source_revision, self.args.team)

    def test_changed_archive_is_rechecked_before_profile_or_signing(self):
        calls, _, _ = self.signing_context()
        signing.verify_source.side_effect = lambda *_: self.archive.write_bytes(b"changed after metadata verification") or self.source
        with self.assertRaisesRegex(ValueError, "changed before signing"):
            signing.sign(self.args)
        signing.packaging.decode_profile.assert_not_called()
        self.assertFalse(any(call[0] == "codesign" for call in calls))

    def test_failed_final_verification_does_not_publish_partial_app(self):
        _, _, signed = self.signing_context()
        signed.side_effect = ValueError("final signature invalid")
        with self.assertRaisesRegex(ValueError, "signature invalid"):
            signing.sign(self.args)
        self.assertEqual(list((self.folder / "build/fskit-validation/signed").iterdir()), [])

    def test_output_directory_cannot_redirect_to_existing_app_or_other_data(self):
        elsewhere = self.folder / "existing-data"
        elsewhere.mkdir()
        (elsewhere / "keep").write_bytes(b"keep")
        (self.folder / "build").symlink_to(elsewhere, target_is_directory=True)
        with mock.patch.object(signing, "ROOT", self.folder), self.assertRaisesRegex(ValueError, "symlinks"):
            signing.output_base()
        self.assertEqual([path.name for path in elsewhere.iterdir()], ["keep"])

    def test_invalid_identity_is_rejected_before_certificate_export(self):
        with mock.patch.object(signing, "run") as run, self.assertRaisesRegex(ValueError, "fingerprint"):
            signing.identity_certificate("Developer ID name", "FIXTURE123")
        run.assert_not_called()

    def test_output_parents_writable_by_other_users_are_rejected(self):
        build = self.folder / "build"
        build.mkdir(mode=0o777)
        build.chmod(0o777)
        with mock.patch.object(signing, "ROOT", self.folder), self.assertRaisesRegex(ValueError, "owned and controlled"):
            signing.output_base()
        self.assertEqual(list(build.iterdir()), [])

    def test_subprocess_errors_do_not_disclose_output_or_arguments(self):
        result = SimpleNamespace(returncode=1, stdout=b"sensitive", stderr=b"sensitive")
        with mock.patch.object(signing.subprocess, "run", return_value=result), self.assertRaisesRegex(ValueError, "tool failed") as failure:
            signing.run("/path/tool", "sensitive")
        self.assertNotIn("sensitive", str(failure.exception))


if __name__ == "__main__":
    unittest.main()
