# macOS setup for the RepoReach beta

RepoReach targets macOS 13 or later, with separate Apple Silicon and Intel downloads. The current beta requires **macFUSE's kernel backend**, installed separately. The app, its Finder extension, and macFUSE have separate approval steps.

## Install and approve macFUSE

Download macFUSE from its [official website](https://macfuse.io/) or [official releases](https://github.com/macfuse/macfuse/releases), then follow the [kernel backend setup guide](https://github.com/macfuse/macfuse/wiki/Getting-Started#kernel-backend). RepoReach does not install the driver or change macOS security settings.

- **Intel Macs:** approve the macFUSE kernel extension when macOS requests it, then restart as directed.
- **Apple Silicon Macs:** if third-party kernel extensions are not already enabled, setup requires Recovery and Startup Security Utility. Select **Reduced Security** and **Allow user management of kernel extensions from identified developers**, then restart. Approve macFUSE when prompted and complete any further requested restart. This changes the startup disk's security policy; Apple's [instructions explain the setting](https://support.apple.com/guide/mac-help/mchl768f7291/mac).

Follow the location named by the macOS approval alert: Apple documents **Privacy & Security** on earlier versions and **Login Items & Extensions** on macOS 15 or later. See [Apple's extension approval guidance](https://support.apple.com/120363). Managed Macs may require an administrator's approval.

macFUSE's FSKit backend is available from macOS 15.4 and avoids the kernel extension's Recovery requirement, but **this beta does not implement that backend**. Installing or enabling only the FSKit component is insufficient. macFUSE's official setup does not require disabling Gatekeeper or System Integrity Protection.

## Choose the repository folder

Choose a dedicated, empty, writable local folder, such as `~/Repositories` or `~/code/Repositories`. RepoReach refuses a nonempty destination, and its private state directory must remain separate. The beta uses a FUSE mount and has no Apple File Provider requirement to live under `~/Library/CloudStorage`.

Use a folder on the Mac's local internal storage for the first beta. External disks, network locations, cloud-synced folders, and mounts inside another virtual filesystem have not been validated. A folder accepted by the app is not proof that the driver can mount it. If connecting fails, retain the error and check driver approval before moving repository data.

## Open a signed but unnotarized beta

The first beta may carry a Developer ID signature without Apple notarization. Check the download's release manifest for its actual signing and notarization status. A signature identifies the publisher and detects altered signed code; it does not substitute for notarization.

Copy `RepoReach.app` into Applications, then try to open it. If macOS blocks it because Apple cannot check it, and you trust the downloaded release:

1. Open **System Settings → Privacy & Security**.
2. In the Security section, choose **Open Anyway** for RepoReach.
3. Confirm **Open** and authenticate if requested.

This saves an exception for the app. Follow [Apple's current opening instructions](https://support.apple.com/102445); do not disable Gatekeeper globally or remove quarantine from unrelated files. If macOS reports damage, malware, or revoked authorization, stop and report the exact warning rather than treating it as the ordinary unnotarized-app prompt.

## Enable Finder actions

Launch the installed app, then enable its Finder extension:

| macOS version | Settings location |
| --- | --- |
| 13 Ventura and 14 Sonoma | **System Settings → Privacy & Security → Extensions**; select RepoReach under Finder Extensions or its Finder checkbox under Added Extensions. |
| 15 Sequoia and later | **System Settings → General → Login Items & Extensions**; open the information button for Finder or Added Extensions and enable RepoReach's Finder extension. |

Apple provides version-specific instructions for [Ventura](https://support.apple.com/guide/mac-help/change-extensions-settings-mchl8baf92fe/13.0/mac/13.0), [Sonoma](https://support.apple.com/guide/mac-help/change-extensions-settings-mchl8baf92fe/14.0/mac/14.0), and [Sequoia](https://support.apple.com/guide/mac-help/mtusr003/15.0/mac/15.0). Use these paths if the app's settings shortcut does not open the expected panel. Finder extension approval does not approve the macFUSE driver.

Connect the chosen folder and right-click a repository to check for Keep Downloaded, Free Up Space, and Refresh Repository. Actions for selections across different repositories are intentionally absent. A compiled or signed extension still needs installed-app validation of its registration, permissions, badges, and actions.

## Background behavior

Closing the management window leaves RepoReach and its service running, with no menu-bar item. Reopen RepoReach to show the window. Explicitly quitting stops its owned service and disconnects the mount. Launch at login is optional and may require approval in System Settings.

## Backend evidence and validation status

The reviewed source selects `fuse.FUSEImplMacFUSE` in the catalogue mount and pins `github.com/jacobsa/fuse` to `a124548f6da78ddcc3681b4e61868e1e4dadd728`. That adapter invokes `mount_macfuse` and expects the legacy device-file-descriptor transport; see its [pinned Darwin implementation](https://github.com/jacobsa/fuse/blob/a124548f6da78ddcc3681b4e61868e1e4dadd728/mount_darwin.go). It does not integrate `MFMount.framework` or request `backend=fskit`.

macFUSE's [MFMount documentation](https://github.com/macfuse/macfuse/wiki/Getting-Started-%28Developer%29-%E2%80%90-MFMount.framework) states that its FSKit backend “no longer exposes a FUSE device file descriptor as the communication endpoint.” Supporting it requires adapting the mount and transport layer, not a user setting. macFUSE 5.4.0's [release notes](https://github.com/macfuse/macfuse/releases/tag/macfuse-5.4.0) describe newer FSKit support and mount-point handling; they do not establish compatibility with this beta's adapter.

Sources and source configuration were reviewed on October 4, 2026. The macOS 15.4.1 development host has no macFUSE installation, so an actual macOS mount, installed Finder integration, and downloaded-app Gatekeeper flow have not been validated there. The macOS 13 deployment target is not a claim that every supported OS and architecture combination has passed runtime tests. Release notes must distinguish build/signature checks from those remaining installation tests.
