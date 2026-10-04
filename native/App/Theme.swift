import SwiftUI

/// Enough Tools' intentionally quiet, square-edged visual language.
enum ReachTheme {
    static let ink = Color(red: 18 / 255, green: 21 / 255, blue: 28 / 255)
    static let paper = Color(red: 244 / 255, green: 245 / 255, blue: 248 / 255)
    static let white = Color.white
    static let accent = Color(red: 59 / 255, green: 79 / 255, blue: 228 / 255)
    static let hairline = Color(red: 221 / 255, green: 225 / 255, blue: 232 / 255)
    static let muted = Color(red: 98 / 255, green: 107 / 255, blue: 122 / 255)
    static let success = Color(red: 38 / 255, green: 105 / 255, blue: 72 / 255)
    static let danger = Color(red: 160 / 255, green: 47 / 255, blue: 45 / 255)

    static func heading(_ size: CGFloat) -> Font {
        .custom("Georgia", size: size)
    }

    static let sourceURL = URL(string: "https://github.com/enoughtools/reporeach")!
    static let websiteURL = URL(string: "https://reporeach.reb.run")!
    static let engineURL = URL(string: "https://github.com/cloudflare/artifact-fs")!

    static func bytes(_ count: Int64) -> String {
        ByteCountFormatter.string(fromByteCount: max(0, count), countStyle: .file)
    }
}

struct ReachButtonStyle: ButtonStyle {
    enum Kind {
        case primary, secondary, quiet, destructive
    }

    var kind: Kind = .secondary
    var compact = false

    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.system(size: compact ? 12 : 13, weight: .semibold))
            .foregroundStyle(foreground)
            .padding(.horizontal, compact ? 11 : 15)
            .padding(.vertical, compact ? 7 : 10)
            .background(background.opacity(configuration.isPressed ? 0.8 : 1))
            .overlay(Rectangle().stroke(border, lineWidth: kind == .quiet ? 0 : 1))
            .contentShape(Rectangle())
            .opacity(isEnabled ? 1 : 0.42)
    }

    private var foreground: Color {
        switch kind {
        case .primary: return ReachTheme.white
        case .destructive: return ReachTheme.danger
        default: return ReachTheme.ink
        }
    }

    private var background: Color {
        kind == .primary ? ReachTheme.accent : kind == .quiet ? .clear : ReachTheme.white
    }

    private var border: Color {
        kind == .primary ? ReachTheme.accent : ReachTheme.hairline
    }
}

struct ReachEyebrow: View {
    let text: String

    var body: some View {
        Text(text.uppercased())
            .font(.system(size: 10, weight: .semibold))
            .tracking(1.6)
            .foregroundStyle(ReachTheme.muted)
    }
}

struct ReachDivider: View {
    var body: some View {
        Rectangle()
            .fill(ReachTheme.hairline)
            .frame(height: 1)
            .accessibilityHidden(true)
    }
}

struct ReachMark: View {
    var size: CGFloat = 36

    var body: some View {
        ZStack {
            Rectangle().fill(ReachTheme.ink)
            Image(systemName: "arrow.down.right")
                .font(.system(size: size * 0.52, weight: .medium))
                .foregroundStyle(.white)
        }
        .frame(width: size, height: size)
        .accessibilityHidden(true)
    }
}
