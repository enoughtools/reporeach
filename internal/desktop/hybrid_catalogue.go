package desktop

// hybridCatalogueEntry describes the leaf shown in the user's ordinary folder.
// An empty LocalPath points at the private virtual catalogue. A LocalPath equal
// to the chosen leaf denotes an ordinary checkout that is never removed here.
type hybridCatalogueEntry struct {
	ID, Owner, Name, LocalPath string
}
