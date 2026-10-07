// Package desktop provides EnoughRepos's local control plane. It never exposes
// credentials over its API and listens only on a private Unix domain socket.
package desktop

const Version = "0.1.0"

type Account struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatarURL,omitempty"`
}

type Repository struct {
	ID              string `json:"id"`
	Owner           string `json:"owner"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	DefaultBranch   string `json:"defaultBranch"`
	Private         bool   `json:"private"`
	HTMLURL         string `json:"htmlURL"`
	CloneURL        string `json:"cloneURL"`
	State           string `json:"state"`
	Pinned          bool   `json:"pinned"`
	DownloadedBytes int64  `json:"downloadedBytes"`
	Error           string `json:"error,omitempty"`
	// Empty Source denotes a catalogue discovered through GitHub in older builds.
	Source   string `json:"source,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	// Local checkouts are ordinary host directories. Adopted directories remain
	// owned by the user; materialized directories were published by Keep.
	LocalPath string `json:"localPath,omitempty"`
	LocalKind string `json:"localKind,omitempty"`
}

// Organization is a catalogue owner group. It can also represent the group
// chosen for a manually adopted repository on another Git host.
type Organization struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type AuthStatus struct {
	Authenticated    bool     `json:"authenticated"`
	Account          *Account `json:"account,omitempty"`
	Pending          bool     `json:"pending"`
	AuthorizationURL string   `json:"authorizationURL,omitempty"`
	DeviceCode       string   `json:"deviceCode,omitempty"`
	Error            string   `json:"error,omitempty"`
}

type Operation struct {
	ID              string `json:"id"`
	RepositoryID    string `json:"repositoryID"`
	Action          string `json:"action"`
	Status          string `json:"status"`
	CompletedBlobs  int64  `json:"completedBlobs"`
	TotalBlobs      int64  `json:"totalBlobs"`
	DownloadedBytes int64  `json:"downloadedBytes"`
	TotalBytes      int64  `json:"totalBytes"`
	CurrentPath     string `json:"currentPath,omitempty"`
	Error           string `json:"error,omitempty"`
}

type Status struct {
	Version   string `json:"version"`
	MountRoot string `json:"mountRoot"`
	// Finder resolves virtual catalogue links to this private volume path.
	// Supplying it as metadata keeps badges and actions independent of file IO.
	VirtualRoot            string         `json:"virtualRoot,omitempty"`
	Mounted                bool           `json:"mounted"`
	MountRecoveryAvailable bool           `json:"mountRecoveryAvailable,omitempty"`
	DependencyReady        bool           `json:"dependencyReady"`
	Account                *Account       `json:"account,omitempty"`
	Repositories           []Repository   `json:"repositories"`
	Operations             []Operation    `json:"operations"`
	Organizations          []Organization `json:"organizations"`
	Message                string         `json:"message,omitempty"`
}
