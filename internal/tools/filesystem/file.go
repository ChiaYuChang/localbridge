package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/ChiaYuChang/local-mcp/internal/tools"
	"github.com/ChiaYuChang/local-mcp/internal/tools/secrets"
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
var ErrTaxonomy = []error{ErrFileOpen, ErrNotAFile, ErrFileTooLarge, ErrInvalidUTF8, ErrFileRead, ErrHiderMissing}

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
	h  *secrets.Hider
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
	if t.h == nil {
		return nil, ToolReadFileO{}, t.formatError(in.Path, fmt.Errorf("read_file: %w", ErrHiderMissing))
	}
	f, err := t.check(in.Path)
	if err != nil {
		return nil, ToolReadFileO{}, t.formatError(in.Path, err)
	}
	defer f.Close()

	bs, err := t.read(f)
	if err != nil {
		return nil, ToolReadFileO{}, t.formatError(in.Path, err)
	}

	masked := t.h.Redact(string(bs))
	return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: masked,
				},
			},
		}, ToolReadFileO{
			Path:    in.Path,
			Content: masked,
			Size:    int64(len(masked)),
		}, nil
}

func (t ToolReadFile) check(path string) (*os.File, error) {
	if secrets.Denied(path) {
		return nil, ErrFileOpen
	}
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
			Description: "Read a UTF-8 text file from the current workspace. Files exceeding the 4 MB text-file limit are rejected as likely non-text and will not be read. Reads under .secrets are denied, and returned content is secret-masked (sizes reflect masked text).",
		},
		t.handle,
	)
	return nil
}

type ToolGetFileInfoI struct {
	Path string `json:"path"`
}

type ToolGetFileInfoO struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
	IsDir   bool      `json:"is_dir"`
}

type ToolGetFileInfo struct {
	fs *FileSystem
}

var _ tools.Tool = ToolGetFileInfo{}

func (t ToolGetFileInfo) Name() string {
	return "get_file_info"
}

func (t ToolGetFileInfo) formatError(path string, err error) error {
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

func (t ToolGetFileInfo) check(path string) (os.FileInfo, error) {
	if secrets.Denied(path) {
		return nil, ErrFileOpen
	}
	f, err := t.fs.root.Open(path)
	if err != nil {
		return nil, ErrFileOpen
	}
	info, err := f.Stat()
	f.Close()
	if err != nil {
		return nil, ErrFileOpen
	}
	return info, nil
}

func (t ToolGetFileInfo) do(info os.FileInfo, rel string) ToolGetFileInfoO {
	return ToolGetFileInfoO{
		Path:    rel,
		Size:    info.Size(),
		Mode:    info.Mode().String(),
		ModTime: info.ModTime(),
		IsDir:   info.IsDir(),
	}
}

func (t ToolGetFileInfo) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolGetFileInfoI,
) (*mcp.CallToolResult, ToolGetFileInfoO, error) {
	info, err := t.check(in.Path)
	if err != nil {
		return nil, ToolGetFileInfoO{}, t.formatError(in.Path, err)
	}

	out := t.do(info, in.Path)
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolGetFileInfo) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Stat a path for size, mode, timestamps and type without reading content (.secrets denied).",
		},
		t.handle,
	)
	return nil
}

type ToolReadMultipleFilesI struct {
	Paths []string `json:"paths"`
}

type FileResult struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Size    int64  `json:"size"`
	Error   string `json:"error,omitempty"`
}

type ToolReadMultipleFilesO struct {
	Files []FileResult `json:"files"`
}

type ToolReadMultipleFiles struct {
	fs *FileSystem
	h  *secrets.Hider
}

var _ tools.Tool = ToolReadMultipleFiles{}

func (t ToolReadMultipleFiles) Name() string {
	return "read_multiple_files"
}

func (t ToolReadMultipleFiles) formatError(path string, err error) error {
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

func (t ToolReadMultipleFiles) handle(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ToolReadMultipleFilesI,
) (*mcp.CallToolResult, ToolReadMultipleFilesO, error) {
	if t.h == nil {
		return nil, ToolReadMultipleFilesO{}, fmt.Errorf("read_multiple_files: %w", ErrHiderMissing)
	}
	if len(in.Paths) < 1 || len(in.Paths) > MaxBatchFiles {
		return nil, ToolReadMultipleFilesO{}, fmt.Errorf("paths length %d out of range [1,%d]", len(in.Paths), MaxBatchFiles)
	}

	single := ToolReadFile{fs: t.fs, h: t.h}
	files := make([]FileResult, 0, len(in.Paths))
	for _, p := range in.Paths {
		fr := FileResult{Path: p}
		f, err := single.check(p)
		if err != nil {
			fr.Error = single.formatError(p, err).Error()
			files = append(files, fr)
			continue
		}
		bs, rerr := single.read(f)
		f.Close()
		if rerr != nil {
			fr.Error = single.formatError(p, rerr).Error()
			files = append(files, fr)
			continue
		}
		masked := t.h.Redact(string(bs))
		fr.Content = masked
		fr.Size = int64(len(masked))
		files = append(files, fr)
	}

	out := ToolReadMultipleFilesO{Files: files}
	text, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(text)},
		},
	}, out, nil
}

func (t ToolReadMultipleFiles) Register(srv *mcp.Server) error {
	mcp.AddTool(
		srv,
		&mcp.Tool{
			Name:        t.Name(),
			Description: "Read up to 50 files in input order with per-file isolated errors. Reads under .secrets are denied, and returned content is secret-masked (sizes reflect masked text).",
		},
		t.handle,
	)
	return nil
}
