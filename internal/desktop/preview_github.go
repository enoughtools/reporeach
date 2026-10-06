package desktop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

const githubPreviewBatchSize = 12
const githubPreviewLimit = 32 << 20

type githubPreviewEntry struct {
	NameRaw string `json:"nameRaw"`
	Type    string `json:"type"`
	Mode    uint32 `json:"mode"`
	OID     string `json:"oid"`
	Size    *int64 `json:"size"`
}

type githubPreviewTree struct {
	OID     string                `json:"oid"`
	Entries *[]githubPreviewEntry `json:"entries"`
}

type githubPreviewRoot struct {
	Commit string
	Tree   githubPreviewTree
}

// SeedGitHubRoots obtains only immediate root entries, in small bounded
// batches. It starts outside filesystem requests, and Acquire joins these
// reservations instead of issuing a second request for a pending repository.
// Already published previews retain their selected commit until cache renewal.
func (c *PreviewCache) SeedGitHubRoots(ctx context.Context, repositories []Repository) error {
	if c.github == nil {
		return nil
	}
	var pending []Repository
	runs := make(map[string]*previewRun)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrPreviewUnavailable
	}
	for _, repo := range repositories {
		if repo.Source == "manual" || repo.Disabled || c.contentPaused[repo.ID] || validateRepository(repo) != nil {
			continue
		}
		key := previewKey(repo)
		if c.items[key] != nil || c.runs[key] != nil {
			continue
		}
		run := &previewRun{done: make(chan struct{}), repoID: repo.ID}
		c.runs[key], runs[key] = run, run
		pending = append(pending, repo)
	}
	if len(pending) != 0 {
		c.wg.Add(1)
	}
	c.mu.Unlock()
	if len(pending) == 0 {
		return ctx.Err()
	}
	defer c.wg.Done()
	seedCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.life, cancel)
	defer func() { stop(); cancel() }()
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	jobs := make(chan []Repository)
	var workers sync.WaitGroup
	var resultMu sync.Mutex
	var result error
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for batch := range jobs {
				err := c.seedBatch(seedCtx, batch, runs)
				if err != nil {
					resultMu.Lock()
					result = errors.Join(result, err)
					resultMu.Unlock()
				}
			}
		}()
	}
	for start := 0; start < len(pending); start += githubPreviewBatchSize {
		end := min(start+githubPreviewBatchSize, len(pending))
		jobs <- pending[start:end]
	}
	close(jobs)
	workers.Wait()
	return result
}

func (c *PreviewCache) seedBatch(ctx context.Context, batch []Repository, runs map[string]*previewRun) error {
	var needed []Repository
	loaded := make(map[string]*RepositoryPreview)
	errorsByKey := make(map[string]error)
	for _, repo := range batch {
		key := previewKey(repo)
		root := filepath.Join(c.root, key)
		if err := privateDirectory(root, true); err != nil {
			errorsByKey[key] = err
			continue
		}
		preview, err := c.load(repo, root)
		if err != nil {
			errorsByKey[key] = err
		} else if preview != nil {
			loaded[key] = preview
		} else {
			needed = append(needed, repo)
		}
	}
	roots, requestErr := c.github.previewRoots(ctx, needed)
	for _, repo := range needed {
		key := previewKey(repo)
		if requestErr != nil {
			errorsByKey[key] = requestErr
			continue
		}
		preview, err := c.installGitHubRoot(ctx, repo, filepath.Join(c.root, key), roots[repo.ID])
		if err != nil {
			errorsByKey[key] = err
		} else {
			loaded[key] = preview
		}
	}
	var result error
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, repo := range batch {
		key := previewKey(repo)
		preview, err := loaded[key], errorsByKey[key]
		if c.closed && preview != nil {
			err = errors.Join(ErrPreviewUnavailable, preview.closeStore())
			preview = nil
		}
		if err == nil {
			c.items[key] = preview
		} else {
			result = errors.Join(result, err)
		}
		run := runs[key]
		run.preview, run.err = preview, err
		delete(c.runs, key)
		close(run.done)
	}
	return result
}

// SelectedCommit performs no acquisition. It is suitable for binding a later
// writable checkout to the same baseline already exposed by a preview.
func (c *PreviewCache) SelectedCommit(repoID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var selected string
	for _, preview := range c.items {
		if preview.repo.ID == repoID {
			if selected != "" && selected != preview.Commit {
				return "" // Source/branch replacement requires an exact source key.
			}
			selected = preview.Commit
		}
	}
	return selected
}

func (c *PreviewCache) SelectedRepositoryCommit(repo Repository) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if preview := c.items[previewKey(repo)]; preview != nil {
		return preview.Commit
	}
	return ""
}

func (g *GitHub) previewRoots(ctx context.Context, repositories []Repository) (map[string]githubPreviewRoot, error) {
	result := make(map[string]githubPreviewRoot)
	if len(repositories) == 0 {
		return result, nil
	}
	var query strings.Builder
	query.WriteString("query {")
	for i, repo := range repositories {
		if validateRepository(repo) != nil || repo.Source == "manual" || repo.DefaultBranch == "" {
			return nil, ErrPreviewUnavailable
		}
		fmt.Fprintf(&query, "r%d: repository(owner:%s,name:%s) { object(expression:%s) { __typename ... on Commit { oid tree { oid entries { nameRaw type mode oid size } } } } }", i,
			strconv.Quote(repo.Owner), strconv.Quote(repo.Name), strconv.Quote("refs/heads/"+repo.DefaultBranch))
	}
	query.WriteString("}")
	var response struct {
		Data map[string]struct {
			Object *struct {
				Type string            `json:"__typename"`
				OID  string            `json:"oid"`
				Tree githubPreviewTree `json:"tree"`
			} `json:"object"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	err := g.api(ctx, 2*time.Minute, githubPreviewLimit, []string{"api", "--hostname", "github.com", "graphql", "-f", "query=" + query.String()}, func(r io.Reader) error {
		if err := decodePreviewJSON(r, &response); err != nil || len(response.Errors) != 0 {
			return errGitHubResponse
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i, repo := range repositories {
		object := response.Data[fmt.Sprintf("r%d", i)].Object
		if object == nil || object.Type != "Commit" || !validPreviewOID(object.OID) || !validPreviewOID(object.Tree.OID) || object.Tree.Entries == nil {
			continue // Missing or empty repos are unavailable, never false empties.
		}
		result[repo.ID] = githubPreviewRoot{Commit: object.OID, Tree: object.Tree}
	}
	return result, nil
}

func (g *GitHub) previewTree(ctx context.Context, repo Repository, oid string) (githubPreviewTree, error) {
	if !validPreviewOID(oid) {
		return githubPreviewTree{}, ErrPreviewUnavailable
	}
	query := fmt.Sprintf("query { repository(owner:%s,name:%s) { object(oid:%s) { __typename ... on Tree { oid entries { nameRaw type mode oid size } } } } }",
		strconv.Quote(repo.Owner), strconv.Quote(repo.Name), strconv.Quote(oid))
	var response struct {
		Data struct {
			Repository struct {
				Object *struct {
					Type string `json:"__typename"`
					githubPreviewTree
				} `json:"object"`
			} `json:"repository"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	err := g.api(ctx, time.Minute, githubPreviewLimit, []string{"api", "--hostname", "github.com", "graphql", "-f", "query=" + query}, func(r io.Reader) error {
		if err := decodePreviewJSON(r, &response); err != nil || len(response.Errors) != 0 {
			return errGitHubResponse
		}
		return nil
	})
	if err != nil {
		return githubPreviewTree{}, err
	}
	object := response.Data.Repository.Object
	if object == nil || object.Type != "Tree" || object.OID != oid || object.Entries == nil {
		return githubPreviewTree{}, errGitHubResponse
	}
	return object.githubPreviewTree, nil
}

func (c *PreviewCache) installGitHubRoot(ctx context.Context, repo Repository, root string, value githubPreviewRoot) (*RepositoryPreview, error) {
	if !validPreviewOID(value.Commit) || !validPreviewOID(value.Tree.OID) || value.Tree.Entries == nil {
		return nil, ErrPreviewUnavailable
	}
	preview, err := c.newPreview(ctx, repo, root, value.Commit, "refs/heads/"+repo.DefaultBranch)
	if err != nil {
		return nil, err
	}
	preview.nodes["."] = model.BaseNode{RepoID: model.RepoID(repo.ID), Path: ".", Type: "dir", Mode: 0o040755,
		ObjectOID: value.Tree.OID, SizeState: "known"}
	if err := preview.addTree(".", value.Tree); err != nil {
		_ = preview.closeStore()
		return nil, err
	}
	if err := preview.publish(ctx); err != nil {
		_ = preview.closeStore()
		return nil, err
	}
	return preview, nil
}

func (p *RepositoryPreview) ensureDirectory(ctx context.Context, path string) error {
	if _, complete := p.trees[path]; complete {
		return nil
	}
	parent := model.CleanPath(filepath.Dir(path))
	if parent == path {
		return ErrPreviewUnavailable
	}
	if err := p.ensureDirectory(ctx, parent); err != nil {
		return err
	}
	node, found := p.nodes[path]
	if !found {
		return os.ErrNotExist
	}
	if node.Type != "dir" {
		return os.ErrInvalid
	}
	if node.Mode == 0o160000 {
		// ArtifactFS represents an uninitialized submodule as an empty folder.
		p.trees[path] = node.ObjectOID
		generation := p.generation
		if err := p.publish(ctx); err != nil {
			if p.generation == generation {
				delete(p.trees, path)
			}
			return err
		}
		return nil
	}
	if p.cache.github == nil {
		return ErrPreviewUnavailable
	}
	tree, err := p.cache.github.previewTree(ctx, p.repo, node.ObjectOID)
	if err != nil {
		return err
	}
	oldNodes, oldTrees, generation := p.nodes, p.trees, p.generation
	p.nodes = make(map[string]model.BaseNode, len(oldNodes)+len(*tree.Entries))
	for path, node := range oldNodes {
		p.nodes[path] = node
	}
	p.trees = make(map[string]string, len(oldTrees)+1)
	for path, oid := range oldTrees {
		p.trees[path] = oid
	}
	if err := p.addTree(path, tree); err != nil {
		p.nodes, p.trees = oldNodes, oldTrees
		return err
	}
	if err := p.publish(ctx); err != nil {
		if p.generation == generation {
			p.nodes, p.trees = oldNodes, oldTrees
		}
		return err
	}
	return nil
}

func (p *RepositoryPreview) addTree(parent string, tree githubPreviewTree) error {
	if !validPreviewOID(tree.OID) || tree.Entries == nil {
		return errGitHubResponse
	}
	entries := make(map[string]model.BaseNode, len(*tree.Entries))
	for _, entry := range *tree.Entries {
		nameBytes, err := base64.StdEncoding.DecodeString(entry.NameRaw)
		name := string(nameBytes) // Git metadata filename, never blob contents.
		if err != nil || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || model.CleanPath(name) != name || !validPreviewOID(entry.OID) {
			return errGitHubResponse
		}
		path := name
		if parent != "." {
			path = parent + "/" + name
		}
		if _, duplicate := entries[path]; duplicate {
			return errGitHubResponse
		}
		node := model.BaseNode{RepoID: model.RepoID(p.repo.ID), Path: path, Mode: entry.Mode, ObjectOID: entry.OID, SizeState: "known"}
		switch {
		case entry.Type == "tree" && entry.Mode == 0o040000:
			node.Type = "dir"
		case entry.Type == "commit" && entry.Mode == 0o160000:
			node.Type = "dir"
		case entry.Type == "blob" && (entry.Mode == 0o100644 || entry.Mode == 0o100755 || entry.Mode == 0o120000):
			if entry.Size == nil || *entry.Size < 0 {
				return errGitHubResponse
			}
			node.Type, node.SizeBytes = "file", *entry.Size
			if entry.Mode == 0o120000 {
				node.Type = "symlink"
			}
		default:
			return errGitHubResponse
		}
		entries[path] = node
	}
	// Publish only after validating every entry. A malformed suffix cannot
	// leave a partial list that would justify an incorrect negative lookup.
	for path, node := range entries {
		p.nodes[path] = node
	}
	p.trees[parent] = tree.OID
	return nil
}

func decodePreviewJSON(r io.Reader, target any) error {
	decoder := json.NewDecoder(r)
	if err := decoder.Decode(target); err != nil {
		return errGitHubResponse
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errGitHubResponse
	}
	return nil
}
