//go:build !darwin

package desktop

func (s *Service) hybridCatalogueMountRoot() (string, error) { return s.state.MountRoot, nil }
func (s *Service) checkHybridCatalogueDirectory(root string) error {
	return s.checkMountDirectory(root)
}
func (s *Service) syncHybridCatalogue(string, []hybridCatalogueEntry) error { return nil }
func (s *Service) releaseHybridCatalogueLink(string, string, string) error  { return nil }
