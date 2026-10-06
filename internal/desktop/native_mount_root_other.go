//go:build !darwin

package desktop

func (s *Service) checkMountDirectory(root string) error { return safeMountDirectory(root) }
