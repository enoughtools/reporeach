import AppKit
import SwiftUI

struct ContentView: View {
    @EnvironmentObject var store: RepositoryStore
    @State private var filter: RepositoryFilter = .all
    @State private var freeCandidate: RepositoryRecord?
    @State private var confirmFree = false
    @State private var showingAdoption = false

    private enum RepositoryFilter: String, CaseIterable {
        case all = "All repositories"
        case kept = "Kept downloaded"
        case active = "In progress"
        case hidden = "Hidden from catalogue"
    }

    private var visibleRepositories: [RepositoryRecord] {
        store.filteredRepositories.filter { repo in
            switch filter {
            case .all: return true
            case .kept: return repo.pinned
            case .active: return repo.isWorking || operation(for: repo)?.isRunning == true
            case .hidden: return !store.isRepositoryEnabled(repo)
            }
        }
    }

    private var selectedRepository: RepositoryRecord? {
        visibleRepositories.first { $0.id == store.selectedRepositoryID }
    }

    var body: some View {
        HStack(spacing: 0) {
            sidebar
            Rectangle().fill(ReachTheme.hairline).frame(width: 1)
            VStack(alignment: .leading, spacing: 0) {
                header
                if store.demoMode { demoNotice }
                if let error = store.errorMessage { errorBanner(error) }
                if let auth = store.authSession, auth.pending { authorizationBanner(auth) }
                if let status = store.status, !status.dependencyReady, !store.demoMode {
                    dependencyBanner
                } else if let status = store.status, !status.mounted,
                          let message = status.message, !message.isEmpty,
                          store.errorMessage == nil, !store.demoMode {
                    mountNotice(message)
                }
                searchBar
                ReachDivider()
                if store.isStarting {
                    startingState
                } else if store.repositories.isEmpty {
                    onboarding
                } else if visibleRepositories.isEmpty {
                    noResults
                } else {
                    HStack(spacing: 0) {
                        repositoryList
                        Rectangle().fill(ReachTheme.hairline).frame(width: 1)
                        detailPane
                            .frame(width: 278)
                    }
                }
                ReachDivider()
                statusBar
            }
            .background(ReachTheme.white)
        }
        .background(ReachTheme.paper)
        .foregroundStyle(ReachTheme.ink)
        .font(.system(size: 13))
        .frame(minWidth: 1040, minHeight: 680)
        .preferredColorScheme(.light)
        .sheet(isPresented: $showingAdoption) { AdoptionView(onAdopted: { filter = .all }).environmentObject(store) }
        .confirmationDialog("Free up space for \(freeCandidate?.name ?? "this repository")?", isPresented: $confirmFree, titleVisibility: .visible) {
            Button("Free Up Space", role: .destructive) {
                if let repo = freeCandidate { Task { await store.action(repo, .free) } }
                freeCandidate = nil
            }
            Button("Cancel", role: .cancel) { freeCandidate = nil }
        } message: {
            Text("RepoReach checks for local changes and unpushed work before reclaiming downloads. If local work exists, the operation will stop. The repository will remain in your catalogue.")
        }
    }

    private var sidebar: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 11) {
                ReachMark()
                VStack(alignment: .leading, spacing: 3) {
                    Text("RepoReach").font(.system(size: 18, weight: .semibold))
                    Text("BY ENOUGH TOOLS").font(.system(size: 9, weight: .medium)).tracking(1.3).foregroundStyle(ReachTheme.muted)
                }
            }
            .padding(.horizontal, 22)
            .padding(.top, 28)
            .padding(.bottom, 34)

            ReachEyebrow(text: "Library").padding(.horizontal, 22).padding(.bottom, 12)
            ForEach(RepositoryFilter.allCases, id: \.self) { item in
                sidebarRow(item.rawValue, symbol: symbol(for: item), count: count(for: item), selected: filter == item && store.ownerFilter == nil) {
                    filter = item
                    store.ownerFilter = nil
                    store.selectedRepositoryID = nil
                }
            }

            if !store.owners.isEmpty {
                ReachEyebrow(text: "Repository groups").padding(.horizontal, 22).padding(.top, 30).padding(.bottom, 12)
                ScrollView {
                    VStack(spacing: 0) {
                        ForEach(store.owners, id: \.self) { owner in
                            sidebarRow(owner, symbol: "building.2", count: store.repositories.filter { $0.owner.caseInsensitiveCompare(owner) == .orderedSame }.count, selected: store.ownerFilter?.caseInsensitiveCompare(owner) == .orderedSame) {
                                filter = .all
                                store.ownerFilter = owner
                                store.selectedRepositoryID = nil
                            }
                            .contextMenu {
                                Button(store.isOwnerEnabled(owner) ? "Hide Group from Catalogue" : "Show Group in Catalogue") {
                                    Task { await store.setOrganizationEnabled(owner, !store.isOwnerEnabled(owner)) }
                                }
                                .disabled(store.isBusy || store.demoMode || store.isOwnerWorking(owner))
                            }
                        }
                    }
                }
                .frame(maxHeight: 260)
            }

            Spacer(minLength: 24)
            VStack(alignment: .leading, spacing: 15) {
                ReachDivider()
                if let account = store.status?.account ?? store.authSession?.account {
                    HStack(spacing: 10) {
                        Image(systemName: "person.crop.square").font(.system(size: 22)).foregroundStyle(ReachTheme.muted)
                        VStack(alignment: .leading, spacing: 3) {
                            Text(account.login).font(.system(size: 12, weight: .semibold)).lineLimit(1)
                            Text("GitHub connected").font(.system(size: 11)).foregroundStyle(ReachTheme.muted)
                        }
                    }
                } else {
                    Button {
                        Task { await store.signIn() }
                    } label: {
                        Label("Connect GitHub", systemImage: "person.badge.key")
                    }
                    .buttonStyle(ReachButtonStyle(compact: true))
                    .disabled(store.isBusy || store.authSession?.pending == true)
                }
                Button { store.showSettings() } label: {
                    Label("Settings", systemImage: "gearshape")
                }
                .buttonStyle(ReachButtonStyle(kind: .quiet, compact: true))
                .padding(.leading, -11)
                .accessibilityIdentifier("open-settings")
            }
            .padding(.horizontal, 22)
            .padding(.bottom, 20)
        }
        .frame(width: 218)
    }

    private func sidebarRow(_ title: String, symbol: String, count: Int, selected: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack(spacing: 10) {
                Image(systemName: symbol).font(.system(size: 13)).frame(width: 17)
                Text(title).lineLimit(1)
                Spacer(minLength: 3)
                Text("\(count)").font(.system(size: 11)).foregroundStyle(selected ? ReachTheme.accent : ReachTheme.muted)
            }
            .font(.system(size: 12, weight: selected ? .semibold : .regular))
            .foregroundStyle(selected ? ReachTheme.accent : ReachTheme.ink)
            .padding(.horizontal, 22)
            .padding(.vertical, 11)
            .background(selected ? ReachTheme.accent.opacity(0.07) : .clear)
            .overlay(alignment: .leading) {
                if selected { Rectangle().fill(ReachTheme.accent).frame(width: 2) }
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityAddTraits(selected ? .isSelected : [])
    }

    private var header: some View {
        HStack(alignment: .center, spacing: 18) {
            VStack(alignment: .leading, spacing: 8) {
                ReachEyebrow(text: store.ownerFilter ?? "Your repositories, on your Mac")
                Text(store.ownerFilter ?? "All your repositories.")
                    .font(ReachTheme.heading(29))
                Text("Within reach. Download only what you need.")
                    .foregroundStyle(ReachTheme.muted)
                    .font(.system(size: 12))
                if let owner = store.ownerFilter {
                    Toggle("Show group in catalogue", isOn: Binding(get: { store.isOwnerEnabled(owner) }, set: { enabled in
                        Task { await store.setOrganizationEnabled(owner, enabled) }
                    }))
                    .toggleStyle(.checkbox)
                    .font(.system(size: 11))
                    .disabled(store.isBusy || store.demoMode || store.isOwnerWorking(owner))
                    .help("Hiding a group pauses downloads and retains its cached data and local work")
                }
            }
            Spacer(minLength: 10)
            Button { showingAdoption = true } label: {
                Label("Add Repository", systemImage: "plus")
            }
            .buttonStyle(ReachButtonStyle(kind: .primary, compact: true))
            .disabled(store.isBusy || store.demoMode || !store.serviceRunning)
            .accessibilityIdentifier("adopt-repository")
            Button {
                Task { await store.discover() }
            } label: {
                Label("Refresh GitHub", systemImage: "arrow.clockwise")
            }
            .buttonStyle(ReachButtonStyle(compact: true))
            .disabled(store.isBusy || store.demoMode || store.status?.account == nil)
            .help("Update the catalogue of repositories your account can access")
        }
        .padding(.horizontal, 28)
        .padding(.top, 28)
        .padding(.bottom, 24)
    }

    private var searchBar: some View {
        HStack(spacing: 11) {
            Image(systemName: "magnifyingglass").foregroundStyle(ReachTheme.muted)
            TextField("Find a repository", text: $store.search)
                .textFieldStyle(.plain)
                .accessibilityIdentifier("repository-search")
            if !store.search.isEmpty {
                Button { store.search = "" } label: {
                    Image(systemName: "xmark.circle.fill").foregroundStyle(ReachTheme.muted)
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Clear search")
            }
            Text("\(visibleRepositories.count) \(visibleRepositories.count == 1 ? "repository" : "repositories")")
                .font(.system(size: 11)).foregroundStyle(ReachTheme.muted)
        }
        .padding(.horizontal, 28)
        .padding(.vertical, 15)
        .background(ReachTheme.paper.opacity(0.55))
    }

    private var repositoryList: some View {
        ScrollView {
            LazyVStack(spacing: 0) {
                ForEach(visibleRepositories) { repo in
                    repositoryRow(repo)
                    ReachDivider().padding(.horizontal, 24)
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private func repositoryRow(_ repo: RepositoryRecord) -> some View {
        let selected = repo.id == store.selectedRepositoryID
        return Button { store.selectedRepositoryID = repo.id } label: {
            HStack(alignment: .top, spacing: 13) {
                Image(systemName: repo.isLocal || repo.pinned ? "folder.fill" : "folder")
                    .font(.system(size: 20, weight: .light))
                    .foregroundStyle(repo.pinned ? ReachTheme.accent : ReachTheme.muted)
                    .frame(width: 24)
                    .padding(.top, 3)
                VStack(alignment: .leading, spacing: 6) {
                    HStack(spacing: 7) {
                        Text(repo.name).font(.system(size: 14, weight: .semibold)).lineLimit(1)
                        if repo.privateRepository {
                            Image(systemName: "lock").font(.system(size: 10)).foregroundStyle(ReachTheme.muted)
                                .accessibilityLabel("Private repository")
                        }
                    }
                    Text(repo.owner).font(.system(size: 11)).foregroundStyle(ReachTheme.muted)
                    if !repo.description.isEmpty {
                        Text(repo.description).font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineLimit(2)
                    }
                    if let operation = operation(for: repo), operation.isRunning {
                        operationProgress(operation, compact: true)
                    }
                }
                Spacer(minLength: 8)
                VStack(alignment: .trailing, spacing: 9) {
                    stateBadge(repo)
                    if repo.downloadedBytes > 0 {
                        Text(ReachTheme.bytes(repo.downloadedBytes))
                            .font(.system(size: 10)).foregroundStyle(ReachTheme.muted)
                    }
                }
                .padding(.top, 2)
            }
            .padding(.horizontal, 24)
            .padding(.vertical, 20)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(selected ? ReachTheme.accent.opacity(0.045) : ReachTheme.white)
            .overlay(alignment: .leading) {
                if selected { Rectangle().fill(ReachTheme.accent).frame(width: 2) }
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityIdentifier("repository-\(repo.id)")
        .accessibilityAddTraits(selected ? .isSelected : [])
    }

    private var detailPane: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                if let repo = selectedRepository {
                    repositoryDetails(repo)
                } else {
                    Image(systemName: "arrow.up.left").font(.system(size: 24, weight: .light)).foregroundStyle(ReachTheme.muted)
                    Text("A place for every project.").font(ReachTheme.heading(23))
                    Text("Select a repository to open its folder, keep it downloaded, or manage its local storage.")
                        .font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineSpacing(4)
                    ReachDivider()
                    ReachEyebrow(text: "How it works")
                    Text("Repositories stay in your catalogue. File contents download as you use them.")
                        .font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineSpacing(4)
                    Text("Keep Downloaded creates an ordinary local checkout that stays available when RepoReach quits. Git history may still need a connection.")
                        .font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineSpacing(4)
                }
            }
            .padding(24)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(ReachTheme.paper.opacity(0.6))
    }

    @ViewBuilder
    private func repositoryDetails(_ repo: RepositoryRecord) -> some View {
        Image(systemName: repo.isLocal || repo.pinned ? "folder.fill" : "folder")
            .font(.system(size: 32, weight: .light))
            .foregroundStyle(ReachTheme.accent)
        VStack(alignment: .leading, spacing: 7) {
            Text(repo.name).font(ReachTheme.heading(24)).fixedSize(horizontal: false, vertical: true)
            Text(repo.owner).foregroundStyle(ReachTheme.muted).font(.system(size: 12))
        }
        if !repo.description.isEmpty {
            Text(repo.description).font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineSpacing(4)
        }
        stateBadge(repo)
        ReachDivider()
        Toggle("Show in catalogue", isOn: Binding(get: { !repo.disabled }, set: { enabled in
            Task { await store.setRepositoryEnabled(repo, enabled) }
        }))
        .toggleStyle(.checkbox)
        .font(.system(size: 12))
        .disabled(store.isRepositoryWorking(repo) || store.isBusy || store.demoMode || operation(for: repo)?.isRunning == true)
        .accessibilityIdentifier("repository-visibility")
        if !store.isOwnerEnabled(repo.owner) {
            Text(repo.isLocal ? "This group is hidden from the catalogue. Your local checkout is retained." : "This group is hidden. Enable it to show this repository in Finder.")
                .font(.system(size: 11)).foregroundStyle(ReachTheme.muted).lineSpacing(3)
            Button("Enable Group") { Task { await store.setOrganizationEnabled(repo.owner, true) } }
                .buttonStyle(ReachButtonStyle(compact: true)).disabled(store.isBusy || store.demoMode || store.isOwnerWorking(repo.owner))
        } else if repo.disabled {
            Text(repo.isLocal ? "This catalogue entry is hidden. Your local checkout stays on your Mac." : "Its virtual folder is hidden and background downloads are paused. Cached data and local work stay on your Mac.")
                .font(.system(size: 11)).foregroundStyle(ReachTheme.muted).lineSpacing(3)
        }
        VStack(spacing: 13) {
            detailValue("Source", value: repo.isManual ? "Added directly" : "GitHub discovery")
            detailValue(repo.isAdopted ? "Branch at adoption" : "Default branch", value: repo.defaultBranch.isEmpty && repo.isAdopted ? "Detached HEAD" : repo.defaultBranch)
            detailValue("Storage", value: repo.isAdopted ? "Original checkout" : repo.isLocal ? "Local checkout" : "On demand")
            if !repo.isLocal { detailValue("Local downloads", value: ReachTheme.bytes(repo.downloadedBytes)) }
            detailValue("Visibility", value: repo.isManual ? "Not provided" : repo.privateRepository ? "Private" : "Public")
        }
        if let localURL = repo.localURL {
            Text(localURL.path).font(.system(size: 11)).foregroundStyle(ReachTheme.muted)
                .textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
        }
        if let error = repo.error, !error.isEmpty {
            Text(error).font(.system(size: 12)).foregroundStyle(ReachTheme.danger).textSelection(.enabled)
        }
        if let operation = operation(for: repo) {
            if operation.isRunning {
                operationProgress(operation)
                Button("Cancel Operation") { Task { await store.action(repo, .cancel) } }
                    .buttonStyle(ReachButtonStyle(compact: true))
            } else if let error = operation.error, !error.isEmpty, error != repo.error {
                Text(error).font(.system(size: 12)).foregroundStyle(ReachTheme.danger).textSelection(.enabled)
            }
        }
        VStack(alignment: .leading, spacing: 10) {
            Button { store.openFolder(repo) } label: {
                Label("Open in Finder", systemImage: "arrow.up.right")
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .buttonStyle(ReachButtonStyle(kind: .primary))
            .disabled((!repo.isLocal && (store.status?.mounted != true || !store.isRepositoryEnabled(repo))) || store.isRepositoryWorking(repo) || store.demoMode)
            .accessibilityIdentifier("open-repository")

            if !repo.pinned {
                Button {
                    Task { await store.action(repo, .keep) }
                } label: {
                    Label("Keep Downloaded", systemImage: "arrow.down.to.line")
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                .buttonStyle(ReachButtonStyle())
                .disabled(store.isRepositoryWorking(repo) || store.isBusy || store.demoMode || !store.isRepositoryEnabled(repo))
                .accessibilityIdentifier("keep-repository")
            }
            Button {
                freeCandidate = repo
                confirmFree = true
            } label: {
                Label("Free Up Space", systemImage: "cloud")
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .buttonStyle(ReachButtonStyle())
            .disabled(!repo.canFreeStorage || store.isRepositoryWorking(repo) || store.isBusy || store.demoMode)
            .accessibilityIdentifier("free-repository")
            .help(repo.isAdopted ? "Your original checkout is retained. RepoReach never removes an adopted folder." : "Return a safely recoverable checkout to an on-demand repository")

            Button {
                Task { await store.action(repo, .refresh) }
            } label: {
                Label("Refresh Repository", systemImage: "arrow.clockwise")
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .buttonStyle(ReachButtonStyle(kind: .quiet, compact: true))
            .disabled(store.isRepositoryWorking(repo) || store.isBusy || store.demoMode || !store.isRepositoryEnabled(repo))
            .padding(.leading, -11)
        }
        Text(repo.isAdopted ? "Your original checkout is retained, including local work. It stays available when RepoReach quits." : repo.isLocal ? "This ordinary local checkout stays available when RepoReach quits." : "Opening files downloads their contents as needed. Keep Downloaded creates a local checkout.")
            .font(.system(size: 11)).foregroundStyle(ReachTheme.muted).lineSpacing(3)
        if let url = URL(string: repo.htmlURL), url.scheme == "https" {
            Link(destination: url) {
                Label("View Repository", systemImage: "arrow.up.right")
                    .font(.system(size: 12, weight: .medium))
            }
            .foregroundStyle(ReachTheme.accent)
        }
    }

    private func detailValue(_ title: String, value: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Text(title).foregroundStyle(ReachTheme.muted)
            Spacer(minLength: 4)
            Text(value).multilineTextAlignment(.trailing).lineLimit(2)
        }
        .font(.system(size: 11))
    }

    private func stateBadge(_ repo: RepositoryRecord) -> some View {
        HStack(spacing: 5) {
            Circle().fill(!store.isRepositoryEnabled(repo) ? ReachTheme.muted : repo.error != nil ? ReachTheme.danger : repo.pinned ? ReachTheme.success : ReachTheme.muted).frame(width: 5, height: 5)
            Text(store.isRepositoryEnabled(repo) ? repo.displayState : "Hidden from catalogue").font(.system(size: 10, weight: .medium))
        }
        .foregroundStyle(repo.error != nil ? ReachTheme.danger : ReachTheme.muted)
        .padding(.horizontal, 7).padding(.vertical, 5)
        .background(ReachTheme.paper)
        .accessibilityElement(children: .combine)
    }

    private func operationProgress(_ operation: EngineOperation, compact: Bool = false) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 8) {
                Text(operation.message ?? operation.action.capitalized)
                    .font(.system(size: compact ? 10 : 11)).foregroundStyle(ReachTheme.muted).lineLimit(2)
                Spacer(minLength: 0)
                if let progress = operation.progress {
                    Text("\(Int(progress * 100))%")
                        .font(.system(size: 10)).foregroundStyle(ReachTheme.muted)
                }
            }
            if let progress = operation.progress {
                ProgressView(value: progress).tint(ReachTheme.accent)
            } else {
                ProgressView().controlSize(.small)
            }
            if let detail = operation.progressDescription {
                Text(detail).font(.system(size: compact ? 10 : 11)).foregroundStyle(ReachTheme.muted)
                    .lineLimit(2)
            }
        }
        .accessibilityElement(children: .combine)
    }

    private var onboarding: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 25) {
                Image(systemName: "folder.badge.plus").font(.system(size: 42, weight: .ultraLight)).foregroundStyle(ReachTheme.accent)
                VStack(alignment: .leading, spacing: 12) {
                    Text("Your projects, without the pile-up.").font(ReachTheme.heading(31))
                    Text("Add a Git remote or existing checkout, or connect GitHub to discover repositories. RepoReach puts them in a folder you choose, ready when you need them.")
                        .font(.system(size: 13)).foregroundStyle(ReachTheme.muted).lineSpacing(5).frame(maxWidth: 480, alignment: .leading)
                }
                VStack(alignment: .leading, spacing: 18) {
                    setupStep(number: "01", title: "Bring a Git repository", detail: "Use a Git remote or existing checkout. GitHub sign-in is optional.") {
                        Button("Add Repository") { showingAdoption = true }
                            .buttonStyle(ReachButtonStyle(kind: .primary, compact: true))
                            .disabled(store.isBusy || store.demoMode || !store.serviceRunning)
                    }
                    setupStep(number: "OR", title: "Connect GitHub for discovery", detail: "Private repositories stay private. GitHub's official CLI handles sign-in and credential storage.") {
                        Button(store.status?.account == nil ? "Connect GitHub" : "Discover Repositories") {
                            Task {
                                if store.status?.account == nil { await store.signIn() }
                                else { await store.discover() }
                            }
                        }
                        .buttonStyle(ReachButtonStyle(kind: .primary, compact: true))
                        .disabled(store.isBusy || store.authSession?.pending == true || store.demoMode)
                    }
                    setupStep(number: "02", title: "Choose your home for repositories", detail: store.status?.mountRoot ?? "Choose a folder, such as Repositories inside your home folder.") {
                        Button("Choose Folder") { store.chooseMountFolder() }
                            .buttonStyle(ReachButtonStyle(compact: true)).disabled(store.isBusy || store.demoMode)
                    }
                    setupStep(number: "03", title: "Make room for your work", detail: "Opening files downloads them. Keep Downloaded creates a local checkout that works without RepoReach.") {
                        Button("Open Settings") { store.showSettings() }
                            .buttonStyle(ReachButtonStyle(compact: true))
                    }
                }
            }
            .padding(38)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .frame(maxHeight: .infinity)
    }

    private func setupStep<Action: View>(number: String, title: String, detail: String, @ViewBuilder action: () -> Action) -> some View {
        HStack(alignment: .top, spacing: 16) {
            Text(number).font(ReachTheme.heading(19)).foregroundStyle(ReachTheme.muted).frame(width: 28, alignment: .leading)
            VStack(alignment: .leading, spacing: 7) {
                Text(title).font(.system(size: 13, weight: .semibold))
                Text(detail).font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineSpacing(3).textSelection(.enabled)
                action().padding(.top, 4)
            }
        }
        .frame(maxWidth: 520, alignment: .leading)
    }

    private var noResults: some View {
        VStack(spacing: 14) {
            Image(systemName: "magnifyingglass").font(.system(size: 30, weight: .light)).foregroundStyle(ReachTheme.muted)
            Text(filter == .active && store.search.isEmpty ? "Everything is settled." : "No repositories here.")
                .font(ReachTheme.heading(25))
            Text(filter == .active && store.search.isEmpty ? "Downloads and repository operations will appear here while they run." : "Try another search or choose a different owner or library view.")
                .foregroundStyle(ReachTheme.muted).multilineTextAlignment(.center).frame(maxWidth: 330)
            Button("Show All Repositories") {
                store.search = ""
                store.ownerFilter = nil
                filter = .all
            }
            .buttonStyle(ReachButtonStyle(compact: true))
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .padding(30)
    }

    private var startingState: some View {
        VStack(spacing: 14) {
            ProgressView().controlSize(.small)
            Text("Getting your catalogue ready…").foregroundStyle(ReachTheme.muted)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private var statusBar: some View {
        HStack(spacing: 8) {
            Circle().fill(store.status?.mounted == true ? ReachTheme.success : ReachTheme.muted).frame(width: 6, height: 6)
            Text(store.demoMode ? "Demo catalogue" : store.status?.mounted == true ? "Virtual folders available" : store.serviceRunning ? "Background service running" : "Background service stopped")
                .font(.system(size: 10)).foregroundStyle(ReachTheme.muted)
            if let status = store.status, !status.mountRoot.isEmpty {
                Text("·").foregroundStyle(ReachTheme.hairline)
                Button { store.openFolder() } label: {
                    Text(status.mountRoot).font(.system(size: 10)).lineLimit(1).truncationMode(.middle)
                }
                .buttonStyle(.plain).foregroundStyle(ReachTheme.muted)
                .disabled(store.demoMode)
                .help(status.mountRoot)
            }
            Spacer(minLength: 10)
            if !store.demoMode {
                Button(store.status?.mounted == true ? "Pause Virtual Folders" : "Enable Virtual Folders") {
                    Task {
                        if store.status?.mounted == true { await store.unmount() }
                        else { await store.mount() }
                    }
                }
                .buttonStyle(ReachButtonStyle(kind: .quiet, compact: true))
                .disabled(store.isBusy || store.status?.dependencyReady != true)
            }
        }
        .padding(.horizontal, 20).padding(.vertical, 5)
        .frame(height: 40)
    }

    private var demoNotice: some View {
        notice(symbol: "eye", color: ReachTheme.accent) {
            Text("Demo mode — sample repositories. File and account actions are disabled.").font(.system(size: 11))
        }
    }

    private var dependencyBanner: some View {
        notice(symbol: "externaldrive", color: ReachTheme.ink) {
            VStack(alignment: .leading, spacing: 4) {
                Text("Virtual folders require macOS 26.").font(.system(size: 12, weight: .semibold))
                Text("You can manage repositories here. On macOS 26, enable RepoReach's bundled filesystem extension to show them in Finder.").font(.system(size: 11)).foregroundStyle(ReachTheme.muted)
            }
            Spacer(minLength: 8)
            Button("Filesystem Settings") { store.showFilesystemExtensionSettings() }
                .buttonStyle(ReachButtonStyle(compact: true))
        }
    }

    private func errorBanner(_ message: String) -> some View {
        notice(symbol: "exclamationmark.circle", color: ReachTheme.danger) {
            Text(message).font(.system(size: 12)).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 8)
            Button { Task { await store.refreshStatus() } } label: {
                Text("Check Again")
            }.buttonStyle(ReachButtonStyle(compact: true)).disabled(store.isBusy)
            Button { store.dismissError() } label: {
                Image(systemName: "xmark")
            }.buttonStyle(.plain).accessibilityLabel("Dismiss error")
        }
    }

    private func mountNotice(_ message: String) -> some View {
        notice(symbol: "folder.badge.questionmark", color: ReachTheme.danger) {
            VStack(alignment: .leading, spacing: 4) {
                Text("Repository folders need attention").font(.system(size: 12, weight: .semibold))
                Text(message).font(.system(size: 11)).foregroundStyle(ReachTheme.muted).textSelection(.enabled)
            }
            Spacer(minLength: 8)
            Button("Try Again") { Task { await store.mount() } }
                .buttonStyle(ReachButtonStyle(compact: true)).disabled(store.isBusy)
        }
    }

    private func authorizationBanner(_ auth: AuthSession) -> some View {
        notice(symbol: "person.badge.key", color: ReachTheme.accent) {
            VStack(alignment: .leading, spacing: 5) {
                Text("Finish connecting on GitHub").font(.system(size: 12, weight: .semibold))
                if let code = auth.deviceCode, !code.isEmpty {
                    HStack(spacing: 8) {
                        Text("Enter code:").foregroundStyle(ReachTheme.muted)
                        Text(code).font(.system(size: 12, weight: .semibold)).textSelection(.enabled)
                        Button("Copy") {
                            NSPasteboard.general.clearContents()
                            NSPasteboard.general.setString(code, forType: .string)
                        }.buttonStyle(ReachButtonStyle(kind: .quiet, compact: true))
                    }
                    .font(.system(size: 11))
                } else {
                    Text("Authorize RepoReach in your browser. This window will update when you're connected.")
                        .font(.system(size: 11)).foregroundStyle(ReachTheme.muted)
                }
            }
            Spacer(minLength: 8)
            if let rawURL = auth.authorizationURL, let url = URL(string: rawURL), url.scheme == "https" {
                Link("Open GitHub", destination: url).buttonStyle(ReachButtonStyle(compact: true))
            }
        }
    }

    private func notice<Contents: View>(symbol: String, color: Color, @ViewBuilder content: () -> Contents) -> some View {
        HStack(alignment: .center, spacing: 12) {
            Image(systemName: symbol).foregroundStyle(color).font(.system(size: 16))
            content()
        }
        .padding(.horizontal, 20).padding(.vertical, 12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(color.opacity(0.045))
        .overlay(alignment: .bottom) { ReachDivider() }
    }

    private func operation(for repo: RepositoryRecord) -> EngineOperation? {
        let operations = store.status?.operations.filter { $0.repositoryID == repo.id } ?? []
        return operations.first { $0.isRunning } ?? operations.last
    }

    private func symbol(for filter: RepositoryFilter) -> String {
        switch filter {
        case .all: return "square.grid.2x2"
        case .kept: return "arrow.down.to.line"
        case .active: return "arrow.triangle.2.circlepath"
        case .hidden: return "eye.slash"
        }
    }

    private func count(for filter: RepositoryFilter) -> Int {
        switch filter {
        case .all: return store.repositories.count
        case .kept: return store.repositories.filter(\.pinned).count
        case .active: return store.repositories.filter { $0.isWorking || operation(for: $0)?.isRunning == true }.count
        case .hidden: return store.repositories.filter { !store.isRepositoryEnabled($0) }.count
        }
    }
}

struct SettingsView: View {
    @EnvironmentObject var store: RepositoryStore

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 26) {
                VStack(alignment: .leading, spacing: 8) {
                    ReachEyebrow(text: "RepoReach / Preferences")
                    Text("Make yourself at home.").font(ReachTheme.heading(29))
                    Text("Choose where your projects live and how RepoReach runs.").foregroundStyle(ReachTheme.muted)
                }
                settingSection("Repository folder", symbol: "folder") {
                    Text(store.status?.mountRoot ?? "Loading folder location…")
                        .font(.system(size: 12)).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
                    Text("RepoReach groups repositories by owner inside this folder. Existing checkouts stay in their original locations; kept repositories become ordinary local checkouts.")
                        .font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineSpacing(3)
                    HStack(spacing: 10) {
                        Button("Choose Folder…") { store.chooseMountFolder() }
                            .buttonStyle(ReachButtonStyle(compact: true)).disabled(store.isBusy || store.demoMode)
                        Button("Open Folder") { store.openFolder() }
                            .buttonStyle(ReachButtonStyle(compact: true)).disabled(store.status == nil || store.demoMode)
                    }
                }
                if !store.organizations.isEmpty {
                    settingSection("Owners & organizations", symbol: "building.2") {
                        Text("Choose which groups appear in the virtual catalogue. Hiding a group pauses its background downloads and retains cached data and local checkouts.")
                            .font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineSpacing(3)
                        ForEach(store.organizations) { group in
                            Toggle(group.name, isOn: Binding(get: { store.isOwnerEnabled(group.name) }, set: { enabled in
                                Task { await store.setOrganizationEnabled(group.name, enabled) }
                            }))
                            .toggleStyle(.checkbox)
                            .font(.system(size: 12))
                            .disabled(store.isBusy || store.demoMode || store.isOwnerWorking(group.name))
                            .accessibilityIdentifier("group-visibility-\(group.name)")
                        }
                    }
                }
                settingSection("Background & startup", symbol: "power") {
                    Toggle("Open RepoReach at login", isOn: Binding(get: { store.launchAtLogin }, set: { enabled in Task { await store.setLaunchAtLogin(enabled) } }))
                        .toggleStyle(.checkbox).disabled(store.demoMode)
                    Text("Closing the window keeps virtual folders available. Reopen RepoReach from Applications. Quitting stops virtual folders; adopted and kept local checkouts stay available.")
                        .font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineSpacing(3)
                }
                settingSection("Finder actions", symbol: "macwindow") {
                    Text("Enable RepoReach's Finder extension to use Keep Downloaded and Free Up Space from a repository's context menu.")
                        .font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineSpacing(3)
                    Button("Open Extension Settings") { store.showFinderExtensionSettings() }
                        .buttonStyle(ReachButtonStyle(compact: true)).disabled(store.demoMode)
                }
                settingSection("Filesystem support", symbol: "externaldrive") {
                    HStack(spacing: 7) {
                        Circle().fill(store.status?.dependencyReady == true ? ReachTheme.success : ReachTheme.muted).frame(width: 6, height: 6)
                        Text(store.status?.dependencyReady == true ? "macOS 26 filesystem support available" : "Virtual folders require macOS 26").font(.system(size: 12, weight: .semibold))
                    }
                    Text("Enable RepoReach in System Settings → General → Login Items & Extensions → File System Extensions, then choose Enable Virtual Folders. The filesystem extension is included in the app.")
                        .font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineSpacing(3)
                    Button("Open Filesystem Extension Settings") { store.showFilesystemExtensionSettings() }
                        .buttonStyle(ReachButtonStyle(compact: true))
                }
                settingSection("Private by design", symbol: "lock") {
                    Text("GitHub's official CLI handles credentials using macOS Keychain when available. Repositories use Git directly with your existing Git and SSH credentials. Your repository contents are not sent to Enough Tools.")
                        .font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineSpacing(3)
                    Text("Free Up Space checks for local work before removing downloaded data. Keep Downloaded covers your current checkout, not all Git history.")
                        .font(.system(size: 12)).foregroundStyle(ReachTheme.muted).lineSpacing(3)
                }
                ReachDivider()
                HStack(alignment: .top) {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("RepoReach").font(.system(size: 13, weight: .semibold))
                        Text("An open source tool by Enough Tools.").font(.system(size: 11)).foregroundStyle(ReachTheme.muted)
                        if let version = store.status?.version {
                            Text("Filesystem engine \(version)").font(.system(size: 10)).foregroundStyle(ReachTheme.muted)
                        }
                    }
                    Spacer()
                    VStack(alignment: .trailing, spacing: 8) {
                        Link("Source & issues ↗", destination: ReachTheme.sourceURL)
                        Link("Website ↗", destination: ReachTheme.websiteURL)
                        Link("Built on ArtifactFS ↗", destination: ReachTheme.engineURL)
                    }
                    .font(.system(size: 11)).foregroundStyle(ReachTheme.accent)
                }
                if store.demoMode {
                    Text("Demo mode is active. Account and filesystem settings are disabled.")
                        .font(.system(size: 11)).foregroundStyle(ReachTheme.muted)
                }
                if let error = store.errorMessage {
                    Text(error).font(.system(size: 12)).foregroundStyle(ReachTheme.danger).textSelection(.enabled)
                }
            }
            .padding(32)
        }
        .font(.system(size: 13))
        .foregroundStyle(ReachTheme.ink)
        .background(ReachTheme.paper)
        .frame(width: 620, height: 700)
        .preferredColorScheme(.light)
    }

    private func settingSection<Contents: View>(_ title: String, symbol: String, @ViewBuilder content: () -> Contents) -> some View {
        HStack(alignment: .top, spacing: 17) {
            Image(systemName: symbol).font(.system(size: 19, weight: .light)).foregroundStyle(ReachTheme.muted).frame(width: 24)
            VStack(alignment: .leading, spacing: 11) {
                Text(title).font(.system(size: 13, weight: .semibold))
                content()
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .padding(20)
        .background(ReachTheme.white)
        .overlay(Rectangle().stroke(ReachTheme.hairline, lineWidth: 1))
    }
}
