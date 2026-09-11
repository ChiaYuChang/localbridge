package filesystem

import "os"

type FileSystem struct {
	root *os.Root
}

func New(root string) (*FileSystem, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}

	return &FileSystem{
		root: r,
	}, nil
}

func (fs *FileSystem) Close() error {
	return fs.root.Close()
}
