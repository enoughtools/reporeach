import AppKit
import SwiftUI

struct AdoptionView: View {
    var onAdopted: (() -> Void)? = nil
    @EnvironmentObject var store: RepositoryStore
    @Environment(\.dismiss) private var dismiss

    @State private var remoteURL = ""
    @State private var owner = ""
    @State private var name = ""
    @State private var branch = ""
    @State private var showsDetails = false
    @State private var isSubmitting = false
    @FocusState private var focusedField: Field?

    private enum Field: Hashable {
        case remote, owner, name, branch
    }

    private var isWorking: Bool { store.isBusy || isSubmitting }
    private var canSubmit: Bool { !trimmed(remoteURL).isEmpty && !isWorking && !store.demoMode }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header
            ReachDivider()
            ScrollView {
                VStack(alignment: .leading, spacing: 24) {
                    remoteField
                    localWorkNotice
                    optionalDetails
                    if store.demoMode {
                        Text("Repository adoption is unavailable in preview mode.")
                            .font(.system(size: 12))
                            .foregroundStyle(ReachTheme.muted)
                    }
                    if let error = store.errorMessage, !error.isEmpty {
                        errorNotice(error)
                    }
                }
                .padding(28)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            ReachDivider()
            footer
        }
        .frame(minWidth: 600, minHeight: 650)
        .background(ReachTheme.white)
        .foregroundStyle(ReachTheme.ink)
        .font(.system(size: 13))
        .preferredColorScheme(.light)
        .interactiveDismissDisabled(isWorking)
        .onAppear { store.dismissError(); focusedField = .remote }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 10) {
            ReachEyebrow(text: "Add to your library")
            Text("Bring a repository.")
                .font(ReachTheme.heading(30))
                .accessibilityAddTraits(.isHeader)
            Text("Any Git remote, or a checkout already on your Mac.")
                .font(.system(size: 12))
                .foregroundStyle(ReachTheme.muted)
        }
        .padding(28)
    }

    private var remoteField: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Git remote or local checkout")
                .font(.system(size: 13, weight: .semibold))
            input("https://host/owner/repository.git", text: $remoteURL, field: .remote)
                .accessibilityLabel("Git remote URL or local checkout path")
                .accessibilityIdentifier("adoption-remote-url")
                .onSubmit { submit() }
            HStack(alignment: .top, spacing: 16) {
                Text("Use a Git remote URL or local checkout. SSH and your existing Git credentials work.")
                    .font(.system(size: 12))
                    .foregroundStyle(ReachTheme.muted)
                    .fixedSize(horizontal: false, vertical: true)
                Spacer(minLength: 0)
                Button("Choose Existing Folder") { chooseFolder() }
                    .buttonStyle(ReachButtonStyle(compact: true))
                    .disabled(isWorking || store.demoMode)
                    .accessibilityIdentifier("adoption-choose-folder")
            }
            Text("No GitHub sign-in is needed. Keep tokens and passwords out of the remote URL.")
                .font(.system(size: 11))
                .foregroundStyle(ReachTheme.muted)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    private var localWorkNotice: some View {
        HStack(alignment: .top, spacing: 12) {
            Image(systemName: "folder.badge.plus")
                .font(.system(size: 20, weight: .light))
                .foregroundStyle(ReachTheme.accent)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 7) {
                Text("Your original checkout stays yours.")
                    .font(.system(size: 13, weight: .semibold))
                Text("Adopting a local folder creates a separate virtual checkout from its committed data. The original folder, including uncommitted changes and staged work, stays untouched.")
                    .font(.system(size: 12))
                    .foregroundStyle(ReachTheme.muted)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(ReachTheme.paper)
        .overlay(Rectangle().stroke(ReachTheme.hairline, lineWidth: 1))
    }

    private var optionalDetails: some View {
        DisclosureGroup(isExpanded: $showsDetails) {
            VStack(alignment: .leading, spacing: 16) {
                Text("Leave these blank to use the repository’s owner, name, and default branch.")
                    .font(.system(size: 12))
                    .foregroundStyle(ReachTheme.muted)
                    .fixedSize(horizontal: false, vertical: true)
                HStack(alignment: .top, spacing: 16) {
                    labeledInput("Owner", placeholder: "Automatic", text: $owner, field: .owner)
                    labeledInput("Repository name", placeholder: "Automatic", text: $name, field: .name)
                }
                labeledInput("Branch", placeholder: "Default branch", text: $branch, field: .branch)
            }
            .padding(.top, 16)
        } label: {
            Text("Optional details")
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(ReachTheme.ink)
        }
        .tint(ReachTheme.accent)
        .accessibilityIdentifier("adoption-optional-details")
    }

    private var footer: some View {
        HStack(spacing: 12) {
            if isWorking {
                ProgressView()
                    .controlSize(.small)
                    .accessibilityLabel("Adopting repository")
                Text("Adding to your library…")
                    .font(.system(size: 12))
                    .foregroundStyle(ReachTheme.muted)
            }
            Spacer(minLength: 12)
            Button("Cancel") { dismiss() }
                .buttonStyle(ReachButtonStyle())
                .keyboardShortcut(.cancelAction)
                .disabled(isWorking)
            Button("Adopt Repository") { submit() }
                .buttonStyle(ReachButtonStyle(kind: .primary))
                .keyboardShortcut(.defaultAction)
                .disabled(!canSubmit)
                .accessibilityIdentifier("adoption-submit")
        }
        .padding(.horizontal, 28)
        .padding(.vertical, 20)
    }

    private func labeledInput(_ label: String, placeholder: String, text: Binding<String>, field: Field) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(label).font(.system(size: 12, weight: .medium))
            input(placeholder, text: text, field: field)
                .accessibilityLabel(label)
                .onSubmit { submit() }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func input(_ placeholder: String, text: Binding<String>, field: Field) -> some View {
        TextField(placeholder, text: text)
            .textFieldStyle(.plain)
            .font(.system(size: 13))
            .padding(11)
            .background(ReachTheme.white)
            .overlay(Rectangle().stroke(focusedField == field ? ReachTheme.accent : ReachTheme.hairline, lineWidth: 1))
            .focused($focusedField, equals: field)
            .disabled(isWorking || store.demoMode)
    }

    private func errorNotice(_ error: String) -> some View {
        Label {
            Text(error).fixedSize(horizontal: false, vertical: true)
        } icon: {
            Image(systemName: "exclamationmark.circle")
        }
        .font(.system(size: 12))
        .foregroundStyle(ReachTheme.danger)
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(ReachTheme.danger.opacity(0.05))
        .overlay(Rectangle().stroke(ReachTheme.danger.opacity(0.2), lineWidth: 1))
        .accessibilityIdentifier("adoption-error")
    }

    private func chooseFolder() {
        let panel = NSOpenPanel()
        panel.title = "Choose an existing Git checkout"
        panel.prompt = "Choose Folder"
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.canCreateDirectories = false
        if panel.runModal() == .OK, let folder = panel.url {
            remoteURL = folder.path
            focusedField = .remote
        }
    }

    private func submit() {
        guard canSubmit else { return }
        isSubmitting = true
        Task {
            let accepted = await store.adoptRepository(
                remoteURL: trimmed(remoteURL),
                owner: optional(owner),
                name: optional(name),
                branch: optional(branch)
            )
            isSubmitting = false
            if accepted { onAdopted?(); dismiss() }
        }
    }

    private func trimmed(_ value: String) -> String {
        value.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private func optional(_ value: String) -> String? {
        let value = trimmed(value)
        return value.isEmpty ? nil : value
    }
}
