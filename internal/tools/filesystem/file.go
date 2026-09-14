package filesystem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"unicode/utf8"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	MaxFileSizeMB = 4
	MaxFileSize   = MaxFileSizeMB << 20 // 4MB

	MaxBatchFiles = 50
)

var (
	ErrFileTooLarge = errors.New("file too large")
	ErrFileOpen     = errors.New("cannot open file")
	ErrNotAFile     = errors.New("not a file")
	ErrInvalidUTF8  = errors.New("not valid UTF-8")
	ErrFileRead     = errors.New("cannot read file")
)

// ErrTaxonomy is the membership set for the normalize step in formatError;
// it is never displayed. Unknown errors normalize to ErrFileRead there.
var ErrTaxonomy = []error{ErrFileOpen, ErrNotAFile, ErrFileTooLarge, ErrInvalidUTF8, ErrFileRead}

type ToolReadFileI struct {
	Path string `json:"path"`
}

type ToolReadFileO struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Size    int64  `json:"size"`
}

type ToolReadFile struct {
	fs *FileSystem
}

var _ tools.Tool = ToolReadFile{}

func (t ToolReadFile) Name() string {
	return "read_file"
}

func (t ToolReadFile) formatError(path string, err error) error {
	if !slices.ContainsFunc(ErrTaxonomy, func(s error) bool {
		return errors.Is(err, s)
	}) {
		err = ErrFileRead
	}
	verdict := ""
	if errors.Is(err, ErrFileTooLarge) {
		verdict = fmt.Sprintf("; exceeds the %d MB text-file limit, likely not a text file and will not be read", MaxFileSizeMB)
	}
	return fmt.Errorf("%q: %w%s", path, err, verdict)
}

func (t ToolReadFile) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolReadFileI,
) (*mcp.CallToolResult, ToolReadFileO, error) {
	f, err := t.check(in.Path)
	if err != nil {
		return nil, ToolReadFileO{}, t.formatError(in.Path, err)
	}
	defer f.Close()

	bs, err := t.read(f)
	if err != nil {
		return nil, ToolReadFileO{}, t.formatError(in.Path, err)
	}

	content := string(bs)
	return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: content,
				},
			},
		}, ToolReadFileO{
			Path:    in.Path,
			Content: content,
			Size:    int64(len(bs)),
		}, nil
}

func (t ToolReadFile) check(path string) (*os.File, error) {
	f, err := t.fs.root.Open(path)
	if err != nil {
		return nil, ErrFileOpen
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, ErrFileOpen
	}

	if info.IsDir() {
		f.Close()
		return nil, ErrNotAFile
	}

	if info.Size() > MaxFileSize {
		f.Close()
		return nil, ErrFileTooLarge
	}

	return f, nil
}

func (t ToolReadFile) read(r io.Reader) ([]byte, error) {
	bs, err := io.ReadAll(io.LimitReader(r, MaxFileSize+1))
	if err != nil {
		return nil, ErrFileRead
	}
	if len(bs) > MaxFileSize {
		return nil, ErrFileTooLarge
	}
	if !utf8.Valid(bs) {
		return nil, ErrInvalidUTF8
	}
	return bs, nil
}

func (t ToolReadFile) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Read a UTF-8 text file from the current workspace. Files exceeding the 4 MB text-file limit are rejected as likely non-text and will not be read.",
		},
		t.handle,
	)
	return nil
}
