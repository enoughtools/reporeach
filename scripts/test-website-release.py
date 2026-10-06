#!/usr/bin/env python3
"""Test release downloads in actual Astro production output, using no browser deps.

Builds use private temporary output directories. The real release manifest is
restored even when a build or assertion fails. Node executes the emitted page
script against a small DOM adapter; the test does not reproduce its selection
or rendering logic.
"""

import fcntl
import json
import pathlib
import signal
import subprocess
import tempfile
from dataclasses import dataclass, field
from html.parser import HTMLParser


ROOT = pathlib.Path(__file__).resolve().parents[1]
SITE = ROOT / "site"
VERSION = "9.8.7-ci-fixture"
VOID_ELEMENTS = {"area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr"}


@dataclass
class Element:
    tag: str
    attrs: dict = field(default_factory=dict)
    children: list = field(default_factory=list)

    def text(self):
        return "".join(child.text() if isinstance(child, Element) else child for child in self.children)


class Page(HTMLParser):
    def __init__(self, html):
        super().__init__(convert_charrefs=True)
        self.root = Element("document")
        self.stack = [self.root]
        self.elements = []
        self.ids = {}
        self.feed(html)

    def handle_starttag(self, tag, attrs):
        node = Element(tag, dict(attrs))
        self.stack[-1].children.append(node)
        self.elements.append(node)
        if "id" in node.attrs:
            identifier = node.attrs["id"]
            if identifier in self.ids:
                raise AssertionError(f"Duplicate rendered id: {identifier}")
            self.ids[identifier] = node
        if tag not in VOID_ELEMENTS:
            self.stack.append(node)

    def handle_endtag(self, tag):
        for index in range(len(self.stack) - 1, 0, -1):
            if self.stack[index].tag == tag:
                del self.stack[index:]
                break

    def handle_data(self, data):
        self.stack[-1].children.append(data)

    def by_class(self, name):
        return next(node for node in self.elements if name in node.attrs.get("class", "").split())


def normalized(text):
    return " ".join(text.split())


def release_fixture(backend="macfuse"):
    artifacts = []
    for architecture in ("arm64", "x86_64"):
        for extension in ("dmg", "zip"):
            filename = f"RepoReach-{VERSION}-macOS-{architecture}.{extension}"
            artifacts.append({
                "architecture": architecture, "format": extension,
                "filename": filename,
                "url": f"https://reporeach.reb.run/releases/{VERSION}/{filename}",
                "signature": "developer-id" if backend == "fskit" or architecture == "arm64" else "ad-hoc",
                "notarized": False, "sha256": "0" * 64, "bytes": 1234,
            })
    return {"product": "RepoReach", "version": VERSION, "artifacts": artifacts,
            "filesystemBackend": backend, "minimumMacOS": "13.0",
            "minimumMountMacOS": "26.0" if backend == "fskit" else "13.0"}


def build(output, succeeds=True):
    result = subprocess.run(
        ["npm", "run", "build", "--", "--outDir", str(output)],
        cwd=SITE, text=True, capture_output=True, timeout=120,
    )
    if succeeds and result.returncode:
        raise AssertionError(f"Astro production build failed:\n{result.stdout}\n{result.stderr}")
    if not succeeds:
        if result.returncode == 0:
            raise AssertionError("Malformed release metadata was silently accepted by the production build")
        return None
    return Page((output / "index.html").read_text())


DOM_ADAPTER = r"""
const fs = require('node:fs');
const vm = require('node:vm');
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const elements = new Map();
for (const [id, original] of Object.entries(input.elements)) {
  const element = { ...original, attrs: { ...original.attrs }, listeners: {} };
  element.addEventListener = (type, callback) => { element.listeners[type] = callback; };
  element.getAttribute = (name) => element.attrs[name] ?? null;
  element.setAttribute = (name, value) => { element.attrs[name] = String(value); };
  element.removeAttribute = (name) => { delete element.attrs[name]; };
  element.classList = { remove: (...names) => {
    element.attrs.class = (element.attrs.class ?? '').split(/\s+/).filter(name => !names.includes(name)).join(' ');
  } };
  Object.defineProperty(element, 'href', {
    get: () => element.attrs.href ?? '', set: (value) => { element.attrs.href = String(value); }
  });
  Object.defineProperty(element, 'textContent', {
    get: () => element.text, set: (value) => { element.text = String(value); }
  });
  elements.set(id, element);
}
const document = {
  getElementById: (id) => elements.get(id) ?? null,
  querySelectorAll: (selector) => {
    if (selector !== '[data-demo-repo]') throw new Error(`Unexpected selector: ${selector}`);
    return []; // Demo controls are outside this download regression.
  }
};
vm.runInNewContext(input.script, { document }, { timeout: 1000 });
const architecture = elements.get('architecture');
if (!architecture.listeners.change) throw new Error('Production architecture control has no change handler');
const states = [];
for (const value of ['x86_64', 'arm64']) {
  architecture.value = value;
  architecture.listeners.change({ target: architecture });
  states.push({ architecture: value,
    button: { ...elements.get('download-button').attrs },
    buttonText: elements.get('download-button').text,
    zipURL: elements.get('zip-download').href,
    signing: elements.get('download-detail').text });
}
process.stdout.write(JSON.stringify(states));
"""


def assert_release_page(page, fixture):
    expected = {(item["architecture"], item["format"]): item["url"] for item in fixture["artifacts"]}
    button = page.ids["download-button"]
    assert button.tag == "a", "Download must be a rendered link"
    assert button.attrs.get("href") == expected[("arm64", "dmg")]
    assert button.attrs.get("aria-disabled") != "true" and "disabled" not in button.attrs
    assert "pointer-events-none" not in button.attrs.get("class", "").split()
    assert normalized(button.text()) == "Download disk image"
    assert page.ids["zip-download"].attrs["href"] == expected[("arm64", "zip")]
    assert normalized(page.ids["download-detail"].text()) == "Developer ID signed · Not yet notarized"
    assert VERSION in normalized(page.by_class("download-title").text())
    assert page.ids["checksums"].attrs["href"] == f"/releases/{VERSION}/SHA256SUMS"
    assert ">" not in page.by_class("requirements").text(), "Stray visible markup in requirements"
    requirements = normalized(page.by_class("requirements").text())
    faq = next(normalized(node.text()) for node in page.elements
               if node.tag == "details" and "What does the beta need?" in node.text())
    if fixture["filesystemBackend"] == "fskit":
        assert requirements == "macOS 26+ · Git · Bundled extension · No driver install"
        assert "macOS 26 or later and Git" in faq
        assert "File System Extension in System Settings" in faq
        assert "No separate driver installation is needed" in faq
        assert "macFUSE" not in requirements + faq and "Recovery" not in faq
    else:
        assert requirements == "macOS 13+ · Git · macFUSE kernel backend required"
        assert "macOS 13 or later, Git, and macFUSE's kernel backend" in faq
        assert "Recovery approval and Reduced Security" in faq
    scripts = [node.text() for node in page.elements if node.tag == "script" and "architecture" in node.text() and "download-button" in node.text()]
    assert len(scripts) == 1, "Expected the actual emitted architecture-selection script"
    payload = {"script": scripts[0], "elements": {
        identifier: {"attrs": node.attrs, "text": node.text()}
        for identifier, node in page.ids.items()
    }}
    result = subprocess.run(["node", "-e", DOM_ADAPTER], input=json.dumps(payload),
                            text=True, capture_output=True, timeout=10)
    assert result.returncode == 0, f"Production page script failed: {result.stderr}"
    for state in json.loads(result.stdout):
        architecture = state["architecture"]
        assert state["button"]["href"] == expected[(architecture, "dmg")]
        assert state["zipURL"] == expected[(architecture, "zip")]
        assert state["button"].get("aria-disabled") != "true"
        assert state["buttonText"] == "Download disk image"
        artifact = next(item for item in fixture["artifacts"]
                        if item["architecture"] == architecture and item["format"] == "dmg")
        expected_signing = "Developer ID signed" if artifact["signature"] == "developer-id" else "Unsigned beta"
        assert state["signing"] == f"{expected_signing} · Not yet notarized"


def assert_preview_page(page, expected_version="Beta in development"):
    assert page.ids["download-button"].attrs.get("aria-disabled") == "true"
    assert normalized(page.ids["download-button"].text()) == "Build being prepared"
    assert expected_version in normalized(page.by_class("download-title").text())
    assert "after verification" in normalized(page.ids["download-detail"].text())
    assert "Requirements will accompany the verified download" in page.by_class("requirements").text()
    faq = next(normalized(node.text()) for node in page.elements
               if node.tag == "details" and "What does the beta need?" in node.text())
    assert "Requirements will accompany the verified download" in faq
    assert "macFUSE" not in faq and "File System Extension" not in faq
    assert "disabled" in page.ids["architecture"].attrs


def interrupted(signum, frame):
    raise SystemExit(f"Website regression interrupted by signal {signum}")


def main():
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGHUP, interrupted)
    manifest = SITE / "public/releases/latest.json"
    production_html = SITE / "dist/index.html"
    original_html = production_html.read_bytes() if production_html.exists() else None
    (ROOT / "build").mkdir(exist_ok=True)
    with (ROOT / "build/website-release-test.lock").open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        original = manifest.read_bytes() if manifest.exists() else None
        directory_existed = manifest.parent.exists()
        manifest.parent.mkdir(parents=True, exist_ok=True)
        try:
            with tempfile.TemporaryDirectory(prefix="reporeach-website-") as temporary:
                outputs = pathlib.Path(temporary)
                fixture = release_fixture()
                manifest.write_text(json.dumps(fixture) + "\n")
                assert_release_page(build(outputs / "release"), fixture)
                print("PASS: legacy release requirements, enabled architecture downloads and exact signing states")
                fixture = release_fixture("fskit")
                manifest.write_text(json.dumps(fixture) + "\n")
                assert_release_page(build(outputs / "native"), fixture)
                print("PASS: native macOS 26 requirements, bundled extension approval and no driver installation")
                fixture["artifacts"] = []
                manifest.write_text(json.dumps(fixture) + "\n")
                assert_preview_page(build(outputs / "empty-native"), VERSION)
                print("PASS: native metadata without a download makes no native-release requirements claim")
                manifest.unlink()
                assert_preview_page(build(outputs / "preview"))
                print("PASS: missing release metadata retains the development preview")
                manifest.write_text("{ malformed release metadata\n")
                build(outputs / "malformed", succeeds=False)
                print("PASS: malformed release metadata fails the production build")
        finally:
            if original is None:
                manifest.unlink(missing_ok=True)
                if not directory_existed and not any(manifest.parent.iterdir()):
                    manifest.parent.rmdir()
            else:
                manifest.write_bytes(original)
            current_html = production_html.read_bytes() if production_html.exists() else None
            assert current_html == original_html, "Regression build altered the real site/dist/index.html"
        restored = manifest.read_bytes() if manifest.exists() else None
        assert restored == original, "Real release metadata was not restored"
        print("PASS: real release metadata restored; production HTML unchanged")


if __name__ == "__main__":
    main()
