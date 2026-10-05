import AppKit
import SwiftUI

@main
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate {
    private var mainWindow: NSWindow?
    private var settingsWindow: NSWindow?
    private var store: RepositoryStore!
    private var openingAction = false
    private var presentationTask: Task<Void, Never>?
    private var terminationPending = false

    static func main() {
        let application = NSApplication.shared
        let delegate = AppDelegate()
        application.delegate = delegate
        application.setActivationPolicy(.regular)
        withExtendedLifetime(delegate) { application.run() }
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        let arguments = ProcessInfo.processInfo.arguments
        if store == nil {
            store = RepositoryStore(demoMode: arguments.contains("--demo"))
            store.showSettingsHandler = { [weak self] in self?.showSettings() }
        }
        buildMenu()
        if ProcessInfo.processInfo.environment["XCTestConfigurationFilePath"] != nil { return }
        createMainWindow()
        let backgroundLaunch = arguments.contains("--background") || NSAppleEventManager.shared().currentAppleEvent?.eventID == AEEventID(kAEOpenApplication) &&
            NSAppleEventManager.shared().currentAppleEvent?.paramDescriptor(forKeyword: keyAEPropData)?.enumCodeValue == keyAELaunchedAsLogInItem
        let actionLaunch = NSAppleEventManager.shared().currentAppleEvent?.eventID == AEEventID(kAEGetURL)
        if backgroundLaunch || actionLaunch || openingAction {
            NSApp.setActivationPolicy(.accessory)
        } else {
            // Launch Services can deliver a URL just after didFinishLaunching.
            // Leave time for that event so Finder actions do not open a window.
            presentationTask = Task {
                try? await Task.sleep(nanoseconds: 150_000_000)
                if !self.openingAction { self.showMainWindow() }
                else { NSApp.setActivationPolicy(.accessory) }
            }
        }
        Task { await store.start() }
        if let index = arguments.firstIndex(of: "--screenshot"), arguments.indices.contains(index + 1), store.demoMode {
            let destination = arguments[index + 1]
            Task {
                try? await Task.sleep(nanoseconds: 1_000_000_000)
                self.captureDemoWindow(destination: destination)
                NSApp.terminate(nil)
            }
        }
    }

    private func createMainWindow() {
        guard mainWindow == nil else { return }
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1100, height: 740), styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        window.title = "RepoReach"
        window.minSize = NSSize(width: 1040, height: 680)
        window.isReleasedWhenClosed = false
        window.delegate = self
        window.titlebarAppearsTransparent = true
        window.backgroundColor = NSColor(red: 244/255, green: 245/255, blue: 248/255, alpha: 1)
        window.contentView = NSHostingView(rootView: ContentView().environmentObject(store))
        window.center()
        mainWindow = window
    }

    @objc private func showMainWindow() {
        createMainWindow()
        NSApp.setActivationPolicy(.regular)
        mainWindow?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    @objc private func showSettings() {
        if settingsWindow == nil {
            let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 690, height: 640), styleMask: [.titled, .closable], backing: .buffered, defer: false)
            window.title = "RepoReach Settings"; window.isReleasedWhenClosed = false; window.delegate = self
            window.contentView = NSHostingView(rootView: SettingsView().environmentObject(store))
            window.center(); settingsWindow = window
        }
        NSApp.setActivationPolicy(.regular)
        settingsWindow?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        showMainWindow(); return false
    }
    func application(_ application: NSApplication, open urls: [URL]) {
        openingAction = true
        for url in urls { Task { @MainActor [self] in
            if store == nil { store = RepositoryStore(demoMode: ProcessInfo.processInfo.arguments.contains("--demo")); store.showSettingsHandler = { [weak self] in self?.showSettings() } }
            await store.handleActionURL(url)
        } }
    }
    func windowWillClose(_ notification: Notification) {
        DispatchQueue.main.async { [weak self] in
            guard let self else { return }
            if self.mainWindow?.isVisible != true && self.settingsWindow?.isVisible != true { NSApp.setActivationPolicy(.accessory) }
            #if DEBUG
            if ProcessInfo.processInfo.environment["REPOREACH_LIFECYCLE_TRACE"] == "1" {
                print("RepoReach lifecycle: visible=\(self.mainWindow?.isVisible == true || self.settingsWindow?.isVisible == true), serviceRunning=\(self.store.serviceRunning)")
                fflush(stdout)
            }
            #endif
        }
    }
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        guard let store else { return .terminateNow }
        guard !terminationPending else { return .terminateLater }
        terminationPending = true
        Task {
            let detached = await store.prepareToQuit()
            terminationPending = false
            if !detached { showMainWindow() }
            sender.reply(toApplicationShouldTerminate: detached)
        }
        return .terminateLater
    }
    func applicationWillTerminate(_ notification: Notification) { presentationTask?.cancel(); store?.stop() }

    private func buildMenu() {
        let menu = NSMenu()
        let appItem = NSMenuItem(); menu.addItem(appItem)
        let applicationMenu = NSMenu()
        appItem.submenu = applicationMenu
        applicationMenu.addItem(withTitle: "About RepoReach", action: #selector(showAbout), keyEquivalent: "")
        applicationMenu.addItem(.separator())
        applicationMenu.addItem(withTitle: "Settings…", action: #selector(showSettings), keyEquivalent: ",")
        applicationMenu.addItem(.separator())
        applicationMenu.addItem(withTitle: "Hide RepoReach", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        applicationMenu.addItem(.separator())
        applicationMenu.addItem(withTitle: "Quit RepoReach", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        let editItem = NSMenuItem(title: "Edit", action: nil, keyEquivalent: ""); menu.addItem(editItem)
        let editMenu = NSMenu(title: "Edit"); editItem.submenu = editMenu
        editMenu.addItem(withTitle: "Undo", action: Selector(("undo:")), keyEquivalent: "z")
        editMenu.addItem(withTitle: "Redo", action: Selector(("redo:")), keyEquivalent: "Z")
        editMenu.addItem(.separator())
        editMenu.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        editMenu.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        editMenu.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        editMenu.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        let windowItem = NSMenuItem(title: "Window", action: nil, keyEquivalent: ""); menu.addItem(windowItem)
        let windowMenu = NSMenu(title: "Window"); windowItem.submenu = windowMenu
        windowMenu.addItem(withTitle: "Show RepoReach", action: #selector(showMainWindow), keyEquivalent: "0")
        let helpItem = NSMenuItem(title: "Help", action: nil, keyEquivalent: ""); menu.addItem(helpItem)
        let helpMenu = NSMenu(title: "Help"); helpItem.submenu = helpMenu
        helpMenu.addItem(withTitle: "RepoReach Website", action: #selector(openWebsite), keyEquivalent: "")
        helpMenu.addItem(withTitle: "Source & Issues", action: #selector(openSource), keyEquivalent: "")
        NSApp.mainMenu = menu; NSApp.windowsMenu = windowMenu; NSApp.helpMenu = helpMenu
    }

    @objc private func openWebsite() { NSWorkspace.shared.open(URL(string: "https://reporeach.reb.run")!) }
    @objc private func openSource() { NSWorkspace.shared.open(URL(string: "https://github.com/enoughtools/reporeach")!) }
    @objc private func showAbout() {
        NSApp.orderFrontStandardAboutPanel(options: [.applicationName: "RepoReach", .applicationVersion: Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "0.1.0", .credits: NSAttributedString(string: "An open source tool by Enough Tools.\nBuilt on Cloudflare ArtifactFS.")])
    }

    private func captureDemoWindow(destination: String) {
        guard destination.hasPrefix("/"), let view = mainWindow?.contentView,
              let image = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { return }
        view.cacheDisplay(in: view.bounds, to: image)
        if let png = image.representation(using: .png, properties: [:]) { try? png.write(to: URL(fileURLWithPath: destination), options: .atomic) }
    }
}
