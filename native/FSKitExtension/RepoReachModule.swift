import FSKit

@main
struct RepoReachModule: UnaryFileSystemExtension {
    var fileSystem: RepoReachFileSystem { RepoReachFileSystem() }
}
