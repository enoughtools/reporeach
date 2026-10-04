// Package desktop provides RepoReach's local control plane. It never exposes
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
	Version         string       `json:"version"`
	MountRoot       string       `json:"mountRoot"`
	Mounted         bool         `json:"mounted"`
	DependencyReady bool         `json:"dependencyReady"`
	Account         *Account     `json:"account,omitempty"`
	Repositories    []Repository `json:"repositories"`
	Operations      []Operation  `json:"operations"`
	Message         string       `json:"message,omitempty"`
}
