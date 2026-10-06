package desktop

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cloudflare/artifact-fs/internal/model"
)

const previewMetadataLimit = 256 << 20

type previewReceipt struct {
	Version    int                 `json:"version"`
	Source     string              `json:"source"`
	Commit     string              `json:"commit"`
	Ref        string              `json:"ref"`
	Generation int64               `json:"generation"`
	Complete   bool                `json:"complete"`
	Trees      map[string]string   `json:"trees"` // base64-encoded raw path keys
	Nodes      []previewStoredNode `json:"nodes"`
}

type previewStoredNode struct {
	PathRaw string         `json:"pathRaw"`
	Node    model.BaseNode `json:"node"`
}

func safePreviewFile(path string, create bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		return file.Close()
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o077 != 0 {
		return errors.New("preview metadata must be private regular files owned by the current user")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return errors.New("preview metadata must not share a file with another path")
	}
	return nil
}

func (c *PreviewCache) load(repo Repository, root string) (*RepositoryPreview, error) {
	path := filepath.Join(root, "metadata.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := safePreviewFile(path, false); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Size() > previewMetadataLimit {
		return nil, ErrPreviewUnavailable
	}
	var receipt previewReceipt
	decoder := json.NewDecoder(io.LimitReader(file, previewMetadataLimit+1))
	if err := decoder.Decode(&receipt); err != nil {
		return nil, ErrPreviewUnavailable
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, ErrPreviewUnavailable
	}
	if receipt.Version != 1 || receipt.Source != previewKey(repo) || !validPreviewOID(receipt.Commit) || receipt.Generation < 1 || len(receipt.Nodes) == 0 {
		return nil, ErrPreviewUnavailable
	}
	nodes := make(map[string]model.BaseNode, len(receipt.Nodes))
	for _, stored := range receipt.Nodes {
		pathBytes, err := base64.StdEncoding.DecodeString(stored.PathRaw)
		path, node := string(pathBytes), stored.Node
		if err != nil || node.Path != "" || model.CleanPath(path) != path || path == ".." || strings.HasPrefix(path, "../") || strings.IndexByte(path, 0) >= 0 ||
			(node.Type != "file" && node.Type != "dir" && node.Type != "symlink") ||
			(node.SizeState != "known" && node.SizeState != "unknown") || node.SizeBytes < 0 ||
			(!validPreviewOID(node.ObjectOID) && !(node.Type == "dir" && node.Mode == 0o755 && node.ObjectOID == "")) {
			return nil, ErrPreviewUnavailable
		}
		if _, exists := nodes[path]; exists {
			return nil, ErrPreviewUnavailable
		}
		validMode := node.Type == "dir" && (node.Mode == 0o755 || node.Mode == 0o040000 || node.Mode == 0o040755 || node.Mode == 0o160000) ||
			node.Type == "file" && (node.Mode == 0o100644 || node.Mode == 0o100755) || node.Type == "symlink" && node.Mode == 0o120000
		if !validMode || node.RepoID != model.RepoID(repo.ID) || node.Type == "dir" && (node.SizeState != "known" || node.SizeBytes != 0) ||
			node.SizeState == "unknown" && node.SizeBytes != 0 {
			return nil, ErrPreviewUnavailable
		}
		node.Path = path
		nodes[path] = node
	}
	for path := range nodes {
		if path != "." {
			parent := nodes[model.CleanPath(filepath.Dir(path))]
			if parent.Type != "dir" {
				return nil, ErrPreviewUnavailable
			}
		}
	}
	trees := make(map[string]string, len(receipt.Trees))
	for encoded, oid := range receipt.Trees {
		pathBytes, err := base64.StdEncoding.DecodeString(encoded)
		path := string(pathBytes)
		if err != nil {
			return nil, ErrPreviewUnavailable
		}
		trees[path] = oid
	}
	if nodes["."].Type != "dir" || !receipt.Complete && trees["."] == "" {
		return nil, ErrPreviewUnavailable
	}
	for path, oid := range trees {
		if node := nodes[path]; node.Type != "dir" || node.ObjectOID != oid || !validPreviewOID(oid) {
			return nil, ErrPreviewUnavailable
		}
	}
	if !receipt.Complete {
		for path := range nodes {
			if path != "." && trees[model.CleanPath(filepath.Dir(path))] == "" {
				return nil, ErrPreviewUnavailable
			}
		}
	}
	preview, err := c.newPreview(c.life, repo, root, receipt.Commit, receipt.Ref)
	if err != nil {
		return nil, err
	}
	preview.complete, preview.trees, preview.nodes = receipt.Complete, trees, nodes
	// Republish the validated receipt rather than trusting an independently
	// replaceable database generation to establish authoritative absence.
	if err := preview.publish(c.life); err != nil {
		_ = preview.closeStore()
		return nil, err
	}
	return preview, nil
}

func (p *RepositoryPreview) save() error {
	if p.retired {
		return nil
	}
	path := filepath.Join(p.root, "metadata.json")
	if err := safePreviewFile(path, false); err != nil {
		return err
	}
	file, err := os.CreateTemp(p.root, ".preview-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	defer file.Close()
	receipt := previewReceipt{Version: 1, Source: previewKey(p.repo), Commit: p.Commit, Ref: p.Ref,
		Generation: p.generation, Complete: p.complete, Trees: make(map[string]string)}
	for path, node := range p.nodes {
		node.Path = ""
		receipt.Nodes = append(receipt.Nodes, previewStoredNode{PathRaw: base64.StdEncoding.EncodeToString([]byte(path)), Node: node})
	}
	for path, oid := range p.trees {
		receipt.Trees[base64.StdEncoding.EncodeToString([]byte(path))] = oid
	}
	if err := json.NewEncoder(file).Encode(receipt); err != nil {
		return err
	}
	if info, err := file.Stat(); err != nil || info.Size() > previewMetadataLimit {
		return ErrPreviewUnavailable
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	directory, err := os.Open(p.root)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
