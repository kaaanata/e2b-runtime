package credproxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// Store retains only the node CA and management socket. Native CLIs in the
// separate credential owner are the exclusive writers of account credentials.
type Store struct {
	root  string
	owner Owner
}

func OpenStore(root, ownerSocket string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("credential root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("credential root must be a private 0700 directory")
	}
	owner, err := newOwnerClient(ownerSocket)
	if err != nil {
		return nil, err
	}

	return &Store{root: root, owner: owner}, nil
}

func (s *Store) Current(ctx context.Context, p Provider) (Credential, error) {
	l, err := s.owner.Resolve(ctx, ResolveRequest{Provider: p, Purpose: Control})

	return l.Credential, err
}

func (s *Store) Refresh(ctx context.Context, p Provider) (Credential, error) {
	l, err := s.owner.Resolve(ctx, ResolveRequest{Provider: p, Purpose: Control, Force: true})

	return l.Credential, err
}

func (s *Store) Guest(ctx context.Context, p Provider) (GuestFiles, error) {
	c, err := s.Current(ctx, p)
	if err != nil {
		return GuestFiles{}, err
	}

	return c.GuestFiles()
}

func atomicPrivate(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".private-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()

	return dir.Sync()
}
